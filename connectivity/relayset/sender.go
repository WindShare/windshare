// Package relayset coordinates independent relay endpoint lifecycles. Each
// endpoint retains registration/resume authority; the set only merges ingress.
package relayset

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/transport/relayv2"
)

const MaximumEndpoints = 8

var ErrConfig = errors.New("invalid sender relay set configuration")

type SenderEndpoint interface {
	WaitReady(context.Context) error
	ObserveRegistration(func(senderrelay.RegistrationState))
	Wake()
	Accept(context.Context) (*relayv2.Channel, error)
	StopRecovery()
	Cleanup(context.Context) error
}
type SenderFactory func(context.Context, string) (SenderEndpoint, error)

type Sender struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	mu                    sync.Mutex
	endpoints             []SenderEndpoint
	pendingRegistrations  int
	registrationErrors    []*RegistrationFailure
	routes                map[string]routeRegistration
	stopping              bool
	observer              func(SenderStatus)
	everReady             bool
	firstReady            string
	ready                 chan struct{}
	readyOnce             sync.Once
	incoming              chan *relayv2.Channel
	workers               sync.WaitGroup
	stopOnce, cleanupOnce sync.Once
	cleanupDone           chan struct{}
	cleanupError          error
}

func NewSender(parent context.Context, urls []string, dial SenderFactory) (*Sender, error) {
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
	set := &Sender{ctx: ctx, cancel: cancel, pendingRegistrations: len(urls), ready: make(chan struct{}),
		incoming: make(chan *relayv2.Channel, len(urls)), cleanupDone: make(chan struct{}), routes: make(map[string]routeRegistration, len(urls))}
	for _, url := range urls {
		set.routes[url] = routeRegistration{}
	}
	for _, url := range urls {
		set.workers.Add(1)
		go set.run(url, dial)
	}
	return set, nil
}

func (s *Sender) run(url string, dial SenderFactory) {
	defer s.workers.Done()
	endpoint, err := dial(s.ctx, url)
	s.mu.Lock()
	if endpoint != nil {
		s.endpoints = append(s.endpoints, endpoint)
	}
	s.mu.Unlock()
	if err == nil && endpoint == nil {
		err = ErrConfig
	}
	if err == nil {
		endpoint.ObserveRegistration(func(state senderrelay.RegistrationState) {
			s.setRegistration(url, routeRegistration{state: state})
		})
		err = endpoint.WaitReady(s.ctx)
	}
	s.mu.Lock()
	s.pendingRegistrations--
	if err == nil {
		if s.firstReady == "" {
			s.firstReady = url
		}
	} else {
		s.registrationErrors = append(s.registrationErrors, &RegistrationFailure{URL: url, Cause: err})
	}
	if s.firstReady != "" || s.pendingRegistrations == 0 {
		s.readyOnce.Do(func() { close(s.ready) })
	}
	s.mu.Unlock()
	if endpoint == nil || err != nil {
		s.setRegistration(url, routeRegistration{terminal: true})
		return
	}
	for {
		channel, err := endpoint.Accept(s.ctx)
		if err != nil {
			s.setRegistration(url, routeRegistration{terminal: true})
			return
		}
		if channel == nil {
			s.setRegistration(url, routeRegistration{terminal: true})
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

func (s *Sender) Wake() {
	s.mu.Lock()
	endpoints := append([]SenderEndpoint(nil), s.endpoints...)
	s.mu.Unlock()
	for _, endpoint := range endpoints {
		endpoint.Wake()
	}
}

func (s *Sender) ReadyRelayURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstReady
}

type SenderRegistrations struct {
	Admitted []string
	Rejected []string
}

// Registrations freezes what is known at publication. Pending candidates can
// still become useful after the first relay releases the share link.
func (s *Sender) Registrations() SenderRegistrations {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snapshot SenderRegistrations
	for url, registration := range s.routes {
		if registration.state == senderrelay.RegistrationAvailable {
			snapshot.Admitted = append(snapshot.Admitted, url)
		}
	}
	for _, failure := range s.registrationErrors {
		snapshot.Rejected = append(snapshot.Rejected, failure.URL)
	}
	slices.Sort(snapshot.Admitted)
	slices.Sort(snapshot.Rejected)
	return snapshot
}

func (s *Sender) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-s.ready:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.firstReady != "" {
			return nil
		}
		failures := make([]error, len(s.registrationErrors))
		for index, failure := range s.registrationErrors {
			failures[index] = failure
		}
		return errors.Join(failures...)
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
		s.mu.Lock()
		s.stopping = true
		endpoints := append([]SenderEndpoint(nil), s.endpoints...)
		s.mu.Unlock()
		s.cancel()
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
				cleanup.Go(func() { endpoint.StopRecovery(); failures <- endpoint.Cleanup(ctx) })
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
