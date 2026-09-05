// Package relayset coordinates independent relay endpoint lifecycles. Each
// endpoint retains registration/resume authority; the set only merges ingress.
package relayset

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/transport/relayv2"
)

const MaximumEndpoints = 8

var ErrConfig = errors.New("invalid sender relay set configuration")

type SenderEndpoint interface {
	Accept(context.Context) (*relayv2.Channel, error)
	StopRecovery()
	Cleanup(context.Context) error
}
type SenderDial func(context.Context, string) (SenderEndpoint, error)

type Sender struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	mu                    sync.Mutex
	endpoints             []SenderEndpoint
	initialRemaining      int
	initialErrors         []error
	available             bool
	ready                 chan struct{}
	readyOnce             sync.Once
	incoming              chan *relayv2.Channel
	workers               sync.WaitGroup
	stopOnce, cleanupOnce sync.Once
	cleanupDone           chan struct{}
	cleanupError          error
}

func NewSender(parent context.Context, urls []string, dial SenderDial) (*Sender, error) {
	if parent == nil || dial == nil || len(urls) == 0 || len(urls) > MaximumEndpoints {
		return nil, ErrConfig
	}
	seen := make(map[string]bool, len(urls))
	for _, url := range urls {
		if url == "" || seen[url] {
			return nil, ErrConfig
		}
		seen[url] = true
	}
	ctx, cancel := context.WithCancel(parent)
	set := &Sender{ctx: ctx, cancel: cancel, initialRemaining: len(urls), ready: make(chan struct{}),
		incoming: make(chan *relayv2.Channel, len(urls)), cleanupDone: make(chan struct{})}
	for _, url := range urls {
		set.workers.Add(1)
		go set.run(url, dial)
	}
	return set, nil
}

func (s *Sender) run(url string, dial SenderDial) {
	defer s.workers.Done()
	endpoint, err := dial(s.ctx, url)
	s.mu.Lock()
	s.initialRemaining--
	if err == nil && endpoint != nil {
		s.endpoints = append(s.endpoints, endpoint)
		s.available = true
	} else {
		if err == nil {
			err = ErrConfig
		}
		s.initialErrors = append(s.initialErrors, err)
		if endpoint != nil {
			s.endpoints = append(s.endpoints, endpoint)
		}
	}
	if s.available || s.initialRemaining == 0 {
		s.readyOnce.Do(func() { close(s.ready) })
	}
	s.mu.Unlock()
	if endpoint == nil || err != nil {
		return
	}
	for {
		channel, err := endpoint.Accept(s.ctx)
		if err != nil {
			return
		}
		if channel == nil {
			return
		}
		select {
		case s.incoming <- channel:
		case <-s.ctx.Done():
			_ = channel.Close()
			return
		}
	}
}

func (s *Sender) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-s.ready:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.available {
			return nil
		}
		return errors.Join(s.initialErrors...)
	}
}

func (s *Sender) Accept(ctx context.Context) (*relayv2.Channel, error) {
	// Exhausting one relay's recovery window does not revoke sessions or direct
	// lanes admitted through it. Only the share owner ends aggregate ingress.
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-s.ctx.Done():
		return nil, context.Cause(s.ctx)
	case channel := <-s.incoming:
		return channel, nil
	}
}

func (s *Sender) StopRecovery() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		endpoints := append([]SenderEndpoint(nil), s.endpoints...)
		s.mu.Unlock()
		for _, endpoint := range endpoints {
			endpoint.StopRecovery()
		}
	})
}

func (s *Sender) Cleanup(ctx context.Context) error {
	s.StopRecovery()
	s.cleanupOnce.Do(func() {
		go func() {
			// A late successful dial is joined before taking the final ownership snapshot.
			s.workers.Wait()
			s.mu.Lock()
			endpoints := append([]SenderEndpoint(nil), s.endpoints...)
			s.mu.Unlock()
			failures := make(chan error, len(endpoints))
			var cleanup sync.WaitGroup
			for _, endpoint := range endpoints {
				cleanup.Add(1)
				go func() { defer cleanup.Done(); endpoint.StopRecovery(); failures <- endpoint.Cleanup(ctx) }()
			}
			cleanup.Wait()
			close(failures)
			var causes []error
			for err := range failures {
				causes = append(causes, err)
			}
			for {
				select {
				case channel := <-s.incoming:
					_ = channel.Close()
				default:
					s.cleanupError = errors.Join(causes...)
					close(s.cleanupDone)
					return
				}
			}
		}()
	})
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-s.cleanupDone:
		return s.cleanupError
	}
}
