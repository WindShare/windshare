package peerset

import (
	"context"
	"sync"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/session/turncontrol"
	"github.com/windshare/windshare/core/transfer"
)

type turnControlSession interface {
	ProtocolSessionID() protocolsession.ProtocolSessionID
	SetTURNControlHandler(func(context.Context, []byte) error)
	SendTURNControl(context.Context, []byte) error
}

// ReceiverSession owns controls once for every direct and delegated path in a
// protocol generation. Closing an ICE attempt never closes the download job.
type ReceiverSession struct {
	mu          sync.Mutex
	owner       *PeerSet
	config      ReceiverConfig
	ctx         context.Context
	cancel      context.CancelFunc
	work        sync.WaitGroup
	direct      *Path
	turn        *turnpath.Receiver
	result      Result
	closed      bool
	ready, done chan struct{}
	readyOnce   sync.Once
}

func OpenReceiverSession(parent context.Context, config Config, receiver ReceiverConfig) (*ReceiverSession, error) {
	owner, err := New(config)
	if err != nil || receiver.Factory == nil || receiver.Routes > UserRelayRoutes {
		return nil, ErrConfig
	}
	ctx, cancel := context.WithCancel(parent)
	s := &ReceiverSession{owner: owner, config: receiver, ctx: ctx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	s.startTURN(receiver)
	s.mu.Lock()
	err = s.startDirectLocked()
	s.mu.Unlock()
	if err != nil {
		cancel()
		s.work.Wait()
		s.detachControls()
		return nil, err
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

func (s *ReceiverSession) detachControls() {
	if controls, ok := s.config.Lanes.(PeerPathControlSession); ok {
		controls.SetPeerPathControlHandler(nil)
	}
	if controls, ok := s.config.Lanes.(turnControlSession); ok && s.turn != nil {
		controls.SetTURNControlHandler(nil)
	}
}

func (s *ReceiverSession) startTURN(receiver ReceiverConfig) {
	controls, ok := receiver.Lanes.(turnControlSession)
	if !ok || receiver.Routes != UserRelayRoutes {
		return
	}
	sessionID := controls.ProtocolSessionID()
	serviceBudgets := make(map[string]*Budget)
	s.turn = turnpath.NewReceiver(turnpath.ReceiverConfig{SessionID: [16]byte(sessionID), Now: s.owner.config.Clock.Now,
		Send:    controls.SendTURNControl,
		Observe: receiver.Factory.NativeConnectivity().ObserveTURN,
		NetworkGeneration: func() [16]byte {
			return nativepeer.GenerationIdentity(receiver.Factory.NativeConnectivity().NetworkGeneration())
		},
		Open: func(ctx context.Context, grant turncontrol.Grant, ice iceconfig.Connection) (turnpath.ReceiverPath, error) {
			budget := serviceBudgets[grant.Profile]
			if budget == nil {
				budget = NewBudget(s.owner.config.Clock.Now())
				serviceBudgets[grant.Profile] = budget
			}
			path, err := s.owner.Open(ctx, PathConfig{SessionID: sessionID, PeerPathID: v2signal.PeerPathID(grant.PathID),
				Budget: budget, Native: receiver.Factory.NativeConnectivity(), ICE: ice, Demand: ContentDemand,
				Prepare: func(ctx context.Context, binding v2signal.Binding) (PreparedStarter, error) {
					prepared, err := receiver.Factory.PrepareConnection(ctx, receiver.Lanes, binding, v2peer.ConnectionPath{ICE: ice, ReplacesPeerPathID: grant.Replaces,
						OnTURNRecovery: func(reason turncontrol.RecoveryReason) { s.turn.Recover(grant.PathID, reason) }})
					if err != nil {
						return PreparedStarter{}, err
					}
					return PreparedStarter{Close: prepared.Close, Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
						return receiver.Factory.StartPreparedConnection(ctx, receiver.Signaling, receiver.Lanes, binding, prepared, ice)
					}}, nil
				},
			})
			if err != nil {
				return nil, err
			}
			return receiverTURNPath{path}, nil
		},
		Admitted: func(_ turncontrol.Grant, path turnpath.ReceiverPath) { s.admitted(path.(receiverTURNPath).Path) },
		Measure: func(lane turncontrol.Lane) turnpath.Measurement {
			lanes, ok := receiver.Lanes.(interface{ LaneSet() *transfer.LaneSet })
			if !ok {
				return turnpath.Measurement{}
			}
			measurement := lanes.LaneSet().MeasureLane(transfer.LaneIdentity{ID: lane.ID, Epoch: lane.Epoch})
			return turnpath.Measurement{UsefulBytes: measurement.UsefulBytes, Unhelpful: measurement.Unhelpful,
				BusyTime: measurement.BusyTime, PendingBytes: measurement.PendingBytes}
		},
	})
	// Delegation belongs to the session, even while no direct path is running.
	controls.SetTURNControlHandler(func(ctx context.Context, body []byte) error { return s.turn.Handle(ctx, body) })
	s.turn.SetDemand(receiver.Demand == ContentDemand)
	s.work.Go(func() { s.turn.Run(s.ctx) })
}

type receiverTURNPath struct{ *Path }

func (path receiverTURNPath) Lane() (turncontrol.Lane, bool) {
	lane, live := path.Path.Lane()
	return turncontrol.Lane{ID: lane.ID, Epoch: lane.Epoch}, live
}

func (s *ReceiverSession) startDirectLocked() error {
	path, err := s.owner.openDirect(s.ctx, s.config)
	if err != nil {
		return err
	}
	s.direct = path
	s.work.Go(func() {
		ready := path.Ready()
		for {
			select {
			case <-ready:
				s.admitted(path)
				ready = nil
			case <-path.Done():
				result := path.Result()
				if s.turn == nil || result.Scope == protocolsession.PeerFailureSessionTerminal {
					s.mu.Lock()
					s.result = result
					s.mu.Unlock()
					s.cancel()
				}
				return
			}
		}
	})
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
	if s.turn != nil {
		s.turn.SetDemand(demand == ContentDemand)
	}
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
func (s *ReceiverSession) Lane() (sessionruntime.LaneIdentity, bool) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	for _, path := range s.owner.paths {
		if lane, ok := path.Lane(); ok {
			return lane, true
		}
	}
	return sessionruntime.LaneIdentity{}, false
}
func (s *ReceiverSession) Result() Result { s.mu.Lock(); defer s.mu.Unlock(); return s.result }
func (s *ReceiverSession) Err() error     { return s.Result().Cause }
func (s *ReceiverSession) Close() error   { s.cancel(); <-s.done; return s.Err() }
