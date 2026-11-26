package peerset

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/throughput"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type laneSetReceiverLanes struct {
	*fakeReceiverLanes
	laneSet *transferlanes.Set
}

func (l *laneSetReceiverLanes) LaneSet() *transferlanes.Set { return l.laneSet }

type contentOwnerReceiverLanes struct {
	*laneSetReceiverLanes
}

func (*contentOwnerReceiverLanes) DetachLane(session.LaneIdentity) bool { return false }

// immediateSampleClock fires throughput sample timers synchronously so any
// erroneously launched runContentControl(nil) goroutine panics deterministically.
type immediateSampleClock struct {
	*testClock
	sampleTimers atomic.Int32
}

func (c *immediateSampleClock) NewTimer(d time.Duration) Timer {
	timer := c.testClock.NewTimer(d)
	if d == throughput.SampleInterval {
		c.sampleTimers.Add(1)
		timer.(*testTimer).fire(c.Now())
	}
	return timer
}

func newTestReceiverFactory(t *testing.T, clock Clock) *v2peer.ReceiverFactory {
	t.Helper()
	native := newTestNative(t, nativepeer.Config{
		Monitor:      networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Now:          clock.Now,
		Reachability: reachability.New(reachability.Config{Now: clock.Now}),
	})
	t.Cleanup(func() { _ = native.Close(context.Background()) })
	factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Native: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestOpenReceiverSessionRejectsNilParent(t *testing.T) {
	factory := newTestReceiverFactory(t, newClock())
	var nilCtx context.Context
	if _, err := OpenReceiverSession(nilCtx, Config{}, ReceiverConfig{Factory: factory}); !errors.Is(err, ErrConfig) {
		t.Fatalf("OpenReceiverSession(nil) err = %v, want %v", err, ErrConfig)
	}
}

func TestOpenReceiverSessionSkipsNilLaneSet(t *testing.T) {
	cases := []struct {
		name  string
		lanes func(*fakePathControls) v2peer.ReceiverLaneSession
	}{
		{
			name: "lane set owner",
			lanes: func(controls *fakePathControls) v2peer.ReceiverLaneSession {
				return &laneSetReceiverLanes{
					fakeReceiverLanes: &fakeReceiverLanes{
						fakePathControls: controls,
						sessionID:        protocolsession.ProtocolSessionID{11},
					},
				}
			},
		},
		{
			name: "content lane owner",
			lanes: func(controls *fakePathControls) v2peer.ReceiverLaneSession {
				return &contentOwnerReceiverLanes{
					laneSetReceiverLanes: &laneSetReceiverLanes{
						fakeReceiverLanes: &fakeReceiverLanes{
							fakePathControls: controls,
							sessionID:        protocolsession.ProtocolSessionID{12},
						},
					},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &immediateSampleClock{testClock: newClock()}
			factory := newTestReceiverFactory(t, clock)
			controls := &fakePathControls{sent: make(chan []byte, 32)}
			receiverSession, err := OpenReceiverSession(t.Context(), Config{Clock: clock}, ReceiverConfig{
				Factory:   factory,
				Signaling: blockingReceiverSignaling{},
				Lanes:     tc.lanes(controls),
				Demand:    ContentDemand,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Wait for the path maintenance goroutine to emit initial demand before closing.
			receive(t, controls.sent)
			if err := receiverSession.Close(); err != nil {
				t.Fatalf("Close() err = %v", err)
			}
			if got := clock.sampleTimers.Load(); got != 0 {
				t.Fatalf("content control started with nil LaneSet: sampleTimers=%d", got)
			}
		})
	}
}

func TestOpenReceiverSessionEndsWithoutTURNRuntimeWhenLaneSetAllowsTURN(t *testing.T) {
	clock := newClock()
	factory := newTestReceiverFactory(t, clock)
	sessionID := protocolsession.ProtocolSessionID{13}
	laneSet, err := transferlanes.New(transferlanes.Config{
		ProtocolSessionID:  sessionID,
		ContentRoutePolicy: transferlanes.ContentRouteAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer laneSet.Close()

	controls := &fakePathControls{sent: make(chan []byte, 32)}
	lanes := &contentOwnerReceiverLanes{
		laneSetReceiverLanes: &laneSetReceiverLanes{
			fakeReceiverLanes: &fakeReceiverLanes{
				fakePathControls: controls,
				sessionID:        sessionID,
			},
			laneSet: laneSet,
		},
	}
	receiverSession, err := OpenReceiverSession(t.Context(), Config{Clock: clock}, ReceiverConfig{
		Factory:   factory,
		Signaling: blockingReceiverSignaling{},
		Lanes:     lanes,
		Demand:    ContentDemand,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiverSession.Close()
	if receiverSession.turn != nil {
		t.Fatal("session created TURN manager without turnRuntime")
	}

	for {
		control, err := protocolsession.DecodePeerPathControl(receive(t, controls.sent))
		if err != nil {
			t.Fatal(err)
		}
		if control.Kind == protocolsession.PeerPathDemand && control.PeerPathID == [16]byte(receiverSession.direct.key.path) {
			break
		}
	}

	revokeBody, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
		PeerPathID:          [16]byte(receiverSession.direct.key.path),
		NetworkGenerationID: [16]byte{2},
		ControlSequence:     1,
		Kind:                protocolsession.PeerPathRevoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controls.receive(revokeBody); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-receiverSession.direct.Done():
			goto directDone
		case timer := <-clock.timers:
			if timer.duration == time.Second {
				timer.fire(clock.Now())
			}
		case <-deadline:
			t.Fatal("direct path did not terminate after revoke")
		}
	}
directDone:
	receive(t, receiverSession.Done())
	if receiverSession.Result().Cause != ErrPathTerminal {
		t.Fatalf("session result = %+v, want ErrPathTerminal", receiverSession.Result())
	}
}

type blockingRevokePathControls struct {
	*fakePathControls
	revokeStarted chan struct{}
	releaseRevoke chan struct{}
	revokeOnce    sync.Once
}

func (c *blockingRevokePathControls) SendPeerPathControl(ctx context.Context, body []byte) error {
	control, err := protocolsession.DecodePeerPathControl(body)
	if err == nil && control.Kind == protocolsession.PeerPathRevoke {
		c.revokeOnce.Do(func() { close(c.revokeStarted) })
		select {
		case <-c.releaseRevoke:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.fakePathControls.SendPeerPathControl(ctx, body)
}

type turnEnabledReceiverLanes struct {
	*turnTestRuntime
	controls  PeerPathControlSession
	laneSet   *transferlanes.Set
	sessionID protocolsession.ProtocolSessionID
}

func (l *turnEnabledReceiverLanes) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return l.sessionID
}
func (l *turnEnabledReceiverLanes) LaneSet() *transferlanes.Set { return l.laneSet }
func (l *turnEnabledReceiverLanes) SendPeerPathControl(ctx context.Context, body []byte) error {
	return l.controls.SendPeerPathControl(ctx, body)
}
func (l *turnEnabledReceiverLanes) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	l.controls.SetPeerPathControlHandler(handler)
}
func (l *turnEnabledReceiverLanes) RequestLane(ctx context.Context, _ uint32) (protocolsession.LaneGrant, error) {
	<-ctx.Done()
	return protocolsession.LaneGrant{}, ctx.Err()
}
func (l *turnEnabledReceiverLanes) AttachLane(ctx context.Context, _ protocolsession.LaneGrant, _ protocolsession.FrameChannel, _ transferlanes.Route) (receiversession.LaneAdmissionResult, error) {
	<-ctx.Done()
	return receiversession.LaneAdmissionResult{}, ctx.Err()
}

type firstFailThenBlockReceiverSignaling struct {
	calls atomic.Int32
}

func (s *firstFailThenBlockReceiverSignaling) OpenPeerOperation(ctx context.Context, _ v2peer.ReceiverSignalingOperationBinding, _ []byte) (v2peer.ReceiverSignalingOperation, error) {
	if s.calls.Add(1) == 1 {
		// Returning (nil, nil) triggers ReceiverProvenanceSignalingAdapterContract
		// -> PeerFailurePathTerminal so the initial BrowseDemand path exits into
		// closeResources() and sends an outbound PeerPathRevoke.
		return nil, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestReceiverSessionSetDemandContentSpawnsFreshDirectDuringTeardown(t *testing.T) {
	clock := newClock()
	factory := newTestReceiverFactory(t, clock)
	sessionID := protocolsession.ProtocolSessionID{14}
	laneSet, err := transferlanes.New(transferlanes.Config{
		ProtocolSessionID:  sessionID,
		ContentRoutePolicy: transferlanes.ContentRouteAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer laneSet.Close()

	controls := &blockingRevokePathControls{
		fakePathControls: &fakePathControls{sent: make(chan []byte, 64)},
		revokeStarted:    make(chan struct{}),
		releaseRevoke:    make(chan struct{}),
	}
	lanes := &turnEnabledReceiverLanes{
		turnTestRuntime: &turnTestRuntime{
			lanes:    make(map[session.LaneIdentity]*turnTestLane),
			requests: make(chan protocolsession.TURNDemand, 16),
		},
		controls:  controls,
		laneSet:   laneSet,
		sessionID: sessionID,
	}
	signaling := &firstFailThenBlockReceiverSignaling{}
	receiverSession, err := OpenReceiverSession(t.Context(), Config{Clock: clock}, ReceiverConfig{
		Factory:   factory,
		Signaling: signaling,
		Lanes:     lanes,
		Demand:    BrowseDemand,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-controls.releaseRevoke:
		default:
			close(controls.releaseRevoke)
		}
		_ = receiverSession.Close()
	}()
	if receiverSession.turn == nil {
		t.Fatal("expected TURN manager in ContentRouteAll session")
	}

	oldDirect := receiverSession.direct

	// Wait until oldDirect has left its BrowseDemand loop and is blocked inside
	// closeResources() sending PeerPathRevoke (before close(p.done)).
	receive(t, controls.revokeStarted)
	select {
	case <-oldDirect.Done():
		t.Fatal("oldDirect.Done closed while PeerPathRevoke was still blocked")
	default:
	}

	// SetDemand(ContentDemand) must immediately spawn a replacement direct path
	// without waiting for oldDirect's revoke teardown to finish.
	if err := receiverSession.SetDemand(ContentDemand); err != nil {
		t.Fatalf("SetDemand(ContentDemand) err = %v", err)
	}
	receiverSession.mu.Lock()
	newDirect := receiverSession.direct
	receiverSession.mu.Unlock()
	if newDirect == nil || newDirect == oldDirect || newDirect.key.path == oldDirect.key.path {
		t.Fatalf("expected fresh direct path during teardown, got old=%p new=%p", oldDirect, newDirect)
	}
	if newDirect.currentDemand() != ContentDemand {
		t.Fatalf("new direct demand = %v, want ContentDemand", newDirect.currentDemand())
	}

	// Allow oldDirect to finish closing; the session and new direct path must survive.
	close(controls.releaseRevoke)
	receive(t, oldDirect.Done())

	select {
	case <-receiverSession.Done():
		t.Fatal("session closed when superseded direct path finished")
	default:
	}

	// Verify the fresh direct path emits content demand controls.
	for {
		control, err := protocolsession.DecodePeerPathControl(receive(t, controls.sent))
		if err != nil {
			t.Fatal(err)
		}
		if control.Kind == protocolsession.PeerPathDemand && control.PeerPathID == [16]byte(newDirect.key.path) && control.HoldFor == nativepeer.ControlLifetime {
			break
		}
	}
}

func TestReceiverSessionSetDemandDoesNotRespawnAfterSessionTerminal(t *testing.T) {
	clock := newClock()
	factory := newTestReceiverFactory(t, clock)
	owner, err := New(Config{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sessionID := protocolsession.ProtocolSessionID{15}
	oldDirect := &Path{
		owner:           owner,
		key:             pathKey{session: sessionID, path: v2signal.PeerPathID{1}},
		ctx:             ctx,
		cancel:          func() {},
		demand:          BrowseDemand,
		wake:            make(chan struct{}, 1),
		resourceChanges: make(chan struct{}, 1),
		done:            make(chan struct{}),
		ready:           make(chan struct{}),
	}
	oldDirect.finish(Result{Scope: protocolsession.PeerFailureSessionTerminal, Cause: errors.New("authentication")})

	receiverSession := &ReceiverSession{
		owner: owner,
		config: ReceiverConfig{
			Factory:   factory,
			Signaling: blockingReceiverSignaling{},
			Lanes: &fakeReceiverLanes{
				fakePathControls: &fakePathControls{sent: make(chan []byte, 8)},
				sessionID:        sessionID,
			},
			Demand: BrowseDemand,
		},
		ctx:    ctx,
		cancel: cancel,
		direct: oldDirect,
		turn:   &turnManager{},
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	receiverSession.watchPath(oldDirect)

	if err := receiverSession.SetDemand(ContentDemand); !errors.Is(err, ErrPathTerminal) {
		t.Fatalf("SetDemand(ContentDemand) = %v, want %v", err, ErrPathTerminal)
	}
	if receiverSession.direct != oldDirect {
		t.Fatal("session-terminal failure respawned a replacement direct path")
	}
	owner.mu.Lock()
	pathCount := len(owner.paths)
	owner.mu.Unlock()
	if pathCount != 0 {
		t.Fatalf("owner.paths = %d, want 0", pathCount)
	}

	close(oldDirect.done)
	receiverSession.work.Wait()
	if ctx.Err() == nil || receiverSession.Result().Scope != protocolsession.PeerFailureSessionTerminal {
		t.Fatalf("session did not settle as session-terminal: ctxErr=%v result=%+v", ctx.Err(), receiverSession.Result())
	}
}

func TestPathSetDemandDoesNotResurrectOrphanedNativePathDuringOrAfterClose(t *testing.T) {
	clock := newClock()
	native := newTestNative(t, nativepeer.Config{
		Monitor:      networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Now:          clock.Now,
		Reachability: reachability.New(reachability.Config{Now: clock.Now}),
	})
	defer native.Close(context.Background())

	owner, err := newTestPeerSet(t, Config{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := protocolsession.ProtocolSessionID{16}

	for i := range 20 {
		controls := &fakePathControls{sent: make(chan []byte, 32)}
		attemptStarted := make(chan struct{}, 1)
		demand := ContentDemand
		if i == 0 {
			demand = BrowseDemand
		}
		path, err := owner.Open(t.Context(), PathConfig{
			SessionID:  sessionID,
			PeerPathID: v2signal.PeerPathID{byte(i + 1)},
			Budget:     NewBudget(clock.Now()),
			Demand:     demand,
			Native:     native,
			Controls:   controls,
			Start: func(ctx context.Context, _ v2signal.Binding) (Attempt, error) {
				select {
				case attemptStarted <- struct{}{}:
				default:
				}
				attempt := newAttempt()
				go func() {
					<-ctx.Done()
					_ = attempt.Close()
				}()
				return attempt, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		receive(t, attemptStarted)
		receive(t, controls.sent)

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = path.SetDemand(ContentDemand)
		}()
		go func() {
			defer wg.Done()
			_ = path.SetDemand(NoDemand)
		}()
		go func() {
			defer wg.Done()
			_ = path.Close()
		}()
		wg.Wait()

		if err := path.SetDemand(ContentDemand); !errors.Is(err, ErrPathTerminal) {
			t.Fatalf("iteration %d: post-close SetDemand(ContentDemand) = %v, want %v", i, err, ErrPathTerminal)
		}
		if err := path.SetDemand(NoDemand); err != nil {
			t.Fatalf("iteration %d: post-close SetDemand(NoDemand) = %v, want nil", i, err)
		}
	}

	// All MaximumPaths native slots must remain available, proving no closed path
	// was resurrected in NativePeerConnectivity.paths.
	for i := range nativepeer.MaximumPaths {
		id := v2signal.PeerPathID{0x80, byte(i + 1)}
		if _, err := native.SetDemand([16]byte(sessionID), id, false, false); err != nil {
			t.Fatalf("native path slot %d unavailable (orphaned entry leaked): %v", i, err)
		}
	}
}

func TestPathSetDemandContentWhileActiveNotifiesNativeMaintenance(t *testing.T) {
	clock := newClock()
	native := newTestNative(t, nativepeer.Config{
		Monitor:      networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Now:          clock.Now,
		Reachability: reachability.New(reachability.Config{Now: clock.Now}),
	})
	defer native.Close(context.Background())

	owner, err := newTestPeerSet(t, Config{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	controls := &fakePathControls{sent: make(chan []byte, 32)}
	attemptStarted := make(chan struct{}, 1)
	path, err := owner.Open(t.Context(), PathConfig{
		SessionID:  protocolsession.ProtocolSessionID{17},
		PeerPathID: v2signal.PeerPathID{1},
		Demand:     BrowseDemand,
		Native:     native,
		Controls:   controls,
		Start: func(ctx context.Context, _ v2signal.Binding) (Attempt, error) {
			attempt := newAttempt()
			attemptStarted <- struct{}{}
			go func() {
				<-ctx.Done()
				_ = attempt.Close()
			}()
			return attempt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer path.Close()

	receive(t, attemptStarted)
	initial, err := protocolsession.DecodePeerPathControl(receive(t, controls.sent))
	if err != nil || initial.Kind != protocolsession.PeerPathDemand || initial.HoldFor != 0 {
		t.Fatalf("initial browse demand = %+v, err=%v", initial, err)
	}

	if err := path.SetDemand(ContentDemand); err != nil {
		t.Fatalf("SetDemand(ContentDemand) err = %v", err)
	}
	for {
		control, err := protocolsession.DecodePeerPathControl(receive(t, controls.sent))
		if err != nil {
			t.Fatal(err)
		}
		if control.Kind == protocolsession.PeerPathDemand && control.HoldFor == nativepeer.ControlLifetime {
			break
		}
	}
}
