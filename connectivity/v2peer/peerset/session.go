package peerset

import (
	"context"
	"sync"

	"github.com/windshare/windshare/core/session"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

// ReceiverSession owns controls once for every direct path in a protocol
// generation. Closing an ICE attempt never closes the download job.
type ReceiverSession struct {
	turn        *turnManager
	allowTURN   bool
	mu          sync.Mutex
	owner       *PeerSet
	config      ReceiverConfig
	ctx         context.Context
	cancel      context.CancelFunc
	work        sync.WaitGroup
	direct      *Path
	result      Result
	closed      bool
	ready, done chan struct{}
	readyOnce   sync.Once
}

func OpenReceiverSession(parent context.Context, config Config, receiver ReceiverConfig) (*ReceiverSession, error) {
	owner, err := New(config)
	if err != nil || receiver.Factory == nil {
		return nil, ErrConfig
	}
	ctx, cancel := context.WithCancel(parent)
	s := &ReceiverSession{owner: owner, config: receiver, ctx: ctx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	if owner, ok := receiver.Lanes.(interface{ LaneSet() *transferlanes.Set }); ok && owner.LaneSet() != nil {
		s.allowTURN = owner.LaneSet().AllowsRoute(transferlanes.RouteTURN)
		if controls, ok := receiver.Lanes.(turnRuntime); ok && s.allowTURN {
			s.turn = newTURNManager(s, controls)
		}
	}
	s.attachControls()
	s.mu.Lock()
	err = s.startDirectLocked()
	s.mu.Unlock()
	if err != nil {
		cancel()
		s.work.Wait()
		s.detachControls()
		if s.turn != nil {
			s.turn.close()
		}
		return nil, err
	}
	if lanes, ok := receiver.Lanes.(interface{ LaneSet() *transferlanes.Set }); ok {
		s.work.Go(func() { s.runContentControl(lanes.LaneSet()) })
	}
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		s.closed = true
		if s.result.Cause == nil {
			s.result.Stopped = true
		}
		s.mu.Unlock()
		s.work.Wait()
		s.detachControls()
		close(s.done)
	}()
	return s, nil
}

func (s *ReceiverSession) attachControls() {
	if controls, ok := s.config.Lanes.(PeerPathControlSession); ok {
		s.owner.bindPeerPathControls(receiverSessionID(s.config.Lanes), controls)
	}
}

func (s *ReceiverSession) detachControls() {
	if controls, ok := s.config.Lanes.(PeerPathControlSession); ok {
		controls.SetPeerPathControlHandler(nil)
	}
}

func (s *ReceiverSession) startDirectLocked() error {
	path, err := s.owner.openDirect(s.ctx, s.config)
	if err != nil {
		return err
	}
	s.direct = path
	s.watchPath(path)
	return nil
}
func (s *ReceiverSession) admitted(path *Path) {
	if _, ok := path.Lane(); ok {
		s.readyOnce.Do(func() { close(s.ready) })
	}
}
func (s *ReceiverSession) SetDemand(demand Demand) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrPathTerminal
	}
	s.config.Demand = demand
	if demand == NoDemand {
		s.cancel()
		return nil
	}
	select {
	case <-s.direct.Done():
		if demand == ContentDemand {
			return s.startDirectLocked()
		}
		return nil
	default:
		return s.direct.SetDemand(demand)
	}
}
func (s *ReceiverSession) Ready() <-chan struct{} { return s.ready }
func (s *ReceiverSession) Done() <-chan struct{}  { return s.done }
func (s *ReceiverSession) Lane() (session.LaneIdentity, bool) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	for _, path := range s.owner.paths {
		if lane, ok := path.Lane(); ok {
			return lane, true
		}
	}
	return session.LaneIdentity{}, false
}
func (s *ReceiverSession) Result() Result { s.mu.Lock(); defer s.mu.Unlock(); return s.result }
func (s *ReceiverSession) Err() error     { return s.Result().Cause }
func (s *ReceiverSession) Close() error   { s.cancel(); <-s.done; return s.Err() }
