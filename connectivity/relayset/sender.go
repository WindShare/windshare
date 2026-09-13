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
	WaitReady(context.Context) error
	SetAvailabilityObserver(func(bool))
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
	registrationErrors    []error
	current               map[string]bool
	terminal              map[string]bool
	stopping              bool
	observer              func(SenderAvailability)
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
		incoming: make(chan *relayv2.Channel, len(urls)), cleanupDone: make(chan struct{}), current: make(map[string]bool, len(urls)), terminal: make(map[string]bool, len(urls))}
	for _, url := range urls {
		set.current[url] = false
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
		endpoint.SetAvailabilityObserver(func(available bool) { s.setAvailable(url, available) })
		err = endpoint.WaitReady(s.ctx)
	}
	s.mu.Lock()
	s.pendingRegistrations--
	if err == nil {
		if s.firstReady == "" {
			s.firstReady = url
		}
	} else {
		s.registrationErrors = append(s.registrationErrors, err)
	}
	if s.firstReady != "" || s.pendingRegistrations == 0 {
		s.readyOnce.Do(func() { close(s.ready) })
	}
	s.mu.Unlock()
	if endpoint == nil || err != nil {
		s.endpointEnded(url)
		return
	}
	for {
		channel, err := endpoint.Accept(s.ctx)
		if err != nil {
			s.endpointEnded(url)
			return
		}
		if channel == nil {
			s.endpointEnded(url)
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

// SenderAvailability describes only admission through relays. Existing direct
// transfers have independent lifetimes and must never be inferred from this count.
type SenderAvailability struct {
	Available uint32
	Total     uint32
	EverReady bool
	Terminal  uint32
}

func (s *Sender) availabilityLocked() SenderAvailability {
	state := SenderAvailability{Total: uint32(len(s.current)), EverReady: s.everReady}
	for _, available := range s.current {
		if available {
			state.Available++
		}
	}
	for _, terminal := range s.terminal {
		if terminal {
			state.Terminal++
		}
	}
	return state
}

func (s *Sender) setAvailable(url string, available bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current[url] == available {
		return
	}
	s.current[url] = available
	s.everReady = s.everReady || available
	if !s.stopping && s.ctx.Err() == nil && s.observer != nil {
		s.observer(s.availabilityLocked())
	}
}

func (s *Sender) endpointEnded(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current[url] = false
	s.terminal[url] = true
	if !s.stopping && s.ctx.Err() == nil && s.observer != nil {
		s.observer(s.availabilityLocked())
	}
}

func (s *Sender) ObserveAvailability(observer func(SenderAvailability)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = observer
	if observer != nil {
		observer(s.availabilityLocked())
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
		return errors.Join(s.registrationErrors...)
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
