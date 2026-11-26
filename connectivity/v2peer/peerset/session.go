package peerset

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

// ReceiverSession owns controls once for every direct path in a protocol
// generation. Closing an ICE attempt never closes the download job.
type ReceiverSession struct {
	turn        *turnManager
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
	if err != nil || parent == nil || receiver.Factory == nil {
		return nil, ErrConfig
	}
	ctx, cancel := context.WithCancel(parent)
	s := &ReceiverSession{owner: owner, config: receiver, ctx: ctx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	var laneSet *transferlanes.Set
	if lanes, ok := receiver.Lanes.(interface{ LaneSet() *transferlanes.Set }); ok {
		laneSet = lanes.LaneSet()
	}
	if laneSet != nil && laneSet.AllowsRoute(transferlanes.RouteTURN) {
		if controls, ok := receiver.Lanes.(turnRuntime); ok {
			s.turn = newTURNManager(s, controls, laneSet)
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
	if laneSet != nil {
		s.work.Go(func() { s.runContentControl(laneSet) })
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
	if demand > ContentDemand {
		return ErrConfig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return ErrPathTerminal
	}
	s.config.Demand = demand
	if demand == NoDemand {
		s.cancel()
		return nil
	}
	if err := s.direct.SetDemand(demand); !errors.Is(err, ErrPathTerminal) {
		return err
	}
	// Never respawn direct recovery after a session-terminal failure; the
	// watcher is already canceling the session.
	if s.direct.Result().Scope == protocolsession.PeerFailureSessionTerminal {
		return ErrPathTerminal
	}
	if demand == ContentDemand {
		return s.startDirectLocked()
	}
	return nil
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
