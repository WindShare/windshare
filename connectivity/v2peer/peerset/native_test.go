package peerset

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type fakeNetwork struct{}

func (fakeNetwork) Snapshot(context.Context) (networkstate.State, error) {
	return networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1}}}, nil
}

type fakePathControls struct {
	mu          sync.Mutex
	handler     func(context.Context, []byte) error
	bindCalls   int
	detachCalls int
	sent        chan []byte
}

func (c *fakePathControls) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	c.mu.Lock()
	c.handler = handler
	if handler != nil {
		c.bindCalls++
	} else {
		c.detachCalls++
	}
	c.mu.Unlock()
}
func (c *fakePathControls) SendPeerPathControl(_ context.Context, body []byte) error {
	c.sent <- append([]byte(nil), body...)
	return nil
}
func (c *fakePathControls) receive(body []byte) error {
	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()
	return handler(context.Background(), body)
}
func (c *fakePathControls) counts() (binds, detaches int, attached bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bindCalls, c.detachCalls, c.handler != nil
}

type fakeReceiverLanes struct {
	*fakePathControls
	sessionID protocolsession.ProtocolSessionID
}

func (l *fakeReceiverLanes) ProtocolSessionID() protocolsession.ProtocolSessionID { return l.sessionID }
func (l *fakeReceiverLanes) RequestLane(ctx context.Context, _ uint32) (protocolsession.LaneGrant, error) {
	<-ctx.Done()
	return protocolsession.LaneGrant{}, ctx.Err()
}
func (l *fakeReceiverLanes) AttachLane(ctx context.Context, _ protocolsession.LaneGrant, _ protocolsession.FrameChannel, _ transferlanes.Route) (receiversession.LaneAdmissionResult, error) {
	<-ctx.Done()
	return receiversession.LaneAdmissionResult{}, ctx.Err()
}

type blockingReceiverSignaling struct{}

func (blockingReceiverSignaling) OpenPeerOperation(ctx context.Context, _ v2peer.ReceiverSignalingOperationBinding, _ []byte) (v2peer.ReceiverSignalingOperation, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAuthenticatedPathControlsWakeOnlyTheExistingOwnerAndRevokeRetires(t *testing.T) {
	clock := newClock()
	native := newTestNative(t, nativepeer.Config{Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond), Now: clock.Now, Reachability: reachability.New(reachability.Config{Now: clock.Now})})
	defer native.Close(context.Background())
	controls := &fakePathControls{sent: make(chan []byte, 32)}
	owner, _ := newTestPeerSet(t, Config{Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	path := &Path{owner: owner, config: PathConfig{Native: native, Controls: controls}, key: pathKey{path: v2signal.PeerPathID{1}}, ctx: ctx, cancel: cancel, demand: ContentDemand, resourceActive: true,
		wake: make(chan struct{}, 1), resourceChanges: make(chan struct{}, 1)}
	owner.paths[path.key] = path
	owner.bindPeerPathControls(path.key.session, controls)
	done := make(chan struct{})
	go path.maintainResources(ctx, done)
	first := receive(t, controls.sent)
	control, err := protocolsession.DecodePeerPathControl(first)
	if err != nil || control.Kind != protocolsession.PeerPathDemand || control.HoldFor != nativepeer.ControlLifetime {
		t.Fatal(control, err)
	}
	if binds, detaches, attached := controls.counts(); binds != 1 || detaches != 0 || !attached {
		t.Fatalf("unexpected control handler counts: binds=%d detaches=%d attached=%v", binds, detaches, attached)
	}
	receive(t, clock.timers)
	if err := controls.receive([]byte{0xff}); err != nil {
		t.Fatal(err)
	}
	unknownDemand, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
		PeerPathID: [16]byte{9}, NetworkGenerationID: [16]byte{2}, ControlSequence: 1,
		Kind: protocolsession.PeerPathDemand, ValidFor: time.Minute, HoldFor: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controls.receive(unknownDemand); err != nil {
		t.Fatal(err)
	}
	select {
	case <-path.wake:
		t.Fatal("unknown path control woke existing owner")
	default:
	}
	for i, kind := range []protocolsession.PeerPathControlKind{protocolsession.PeerPathMappingReady, protocolsession.PeerPathNetworkChanged, protocolsession.PeerPathRevoke} {
		control := protocolsession.PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2}, ControlSequence: uint64(i + 1), Kind: kind}
		if kind != protocolsession.PeerPathRevoke {
			control.ValidFor = time.Minute
		}
		body, err := protocolsession.EncodePeerPathControl(control)
		if err != nil {
			t.Fatal(err)
		}
		if err := controls.receive(body); err != nil {
			t.Fatal(err)
		}
		receive(t, path.wake)
		if kind != protocolsession.PeerPathRevoke && !path.consumeMapping() {
			t.Fatal("missing reserved-attempt signal")
		}
		if kind == protocolsession.PeerPathRevoke && path.consumeRestart() == nil {
			t.Fatal("missing generation retirement")
		}
	}
	path.mu.Lock()
	retired := path.retired
	path.mu.Unlock()
	if !retired {
		t.Fatal("authenticated revoke lost")
	}
	cancel()
	receive(t, done)
	for {
		select {
		case body := <-controls.sent:
			control, err := protocolsession.DecodePeerPathControl(body)
			if err != nil || control.Kind != protocolsession.PeerPathDemand {
				t.Fatal("remote control echoed", control, err)
			}
		default:
			return
		}
	}
}
func TestNativeMappingReservationFeedsFreshAttemptWithoutResettingUnitBudget(t *testing.T) {
	clock := newClock()
	owner, _ := newTestPeerSet(t, Config{Clock: clock})
	native := newTestNative(t, nativepeer.Config{Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond), Now: clock.Now, Reachability: reachability.New(reachability.Config{Now: clock.Now})})
	defer native.Close(context.Background())
	attempts := make(chan *fakeAttempt, 4)
	path, err := owner.Open(context.Background(), PathConfig{Demand: ContentDemand, Native: native, StopAfterWave: true, Start: func(context.Context, v2signal.Binding) (Attempt, error) {
		a := newAttempt()
		attempts <- a
		return a, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	first := receive(t, attempts)
	path.mu.Lock()
	path.mappingPending = true
	path.mu.Unlock()
	first.Close()
	// Mapping readiness wakes the existing path; it cannot create an overlapping PC.
	var timer *testTimer
	for timer == nil {
		next := receive(t, clock.timers)
		if next.duration == time.Second {
			timer = next
		}
	}
	timer.fire(clock.Now())
	second := receive(t, attempts)
	close(second.ready)
	receive(t, path.Ready())
	path.mu.Lock()
	path.mappingPending = true
	path.mu.Unlock()
	if err := path.SetDemand(NoDemand); err != nil {
		t.Fatal(err)
	}
	receive(t, path.Done())
	if !path.Result().Stopped {
		t.Fatal(path.Result())
	}
}

func TestReceiverSessionBindsPeerPathControlOnceAcrossParallelPaths(t *testing.T) {
	clock := newClock()
	capacity, err := NewCapacity(DefaultAttemptCapacity)
	if err != nil {
		t.Fatal(err)
	}
	native := newTestNative(t, nativepeer.Config{
		Monitor:      networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Now:          clock.Now,
		Reachability: reachability.New(reachability.Config{Now: clock.Now}),
	})
	factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Native: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	controls := &fakePathControls{sent: make(chan []byte, 32)}
	lanes := &fakeReceiverLanes{
		fakePathControls: controls,
		sessionID:        protocolsession.ProtocolSessionID{7},
	}
	session, err := OpenReceiverSession(t.Context(), Config{Clock: clock, Capacity: capacity}, ReceiverConfig{
		Factory:   factory,
		Signaling: blockingReceiverSignaling{},
		Lanes:     lanes,
		Demand:    ContentDemand,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// Session controls must be bound synchronously once on open.
	if binds, detaches, attached := controls.counts(); binds != 1 || detaches != 0 || !attached {
		t.Fatalf("after open: binds=%d detaches=%d attached=%v", binds, detaches, attached)
	}
	waitDemand := func(target v2signal.PeerPathID) {
		t.Helper()
		for {
			control, err := protocolsession.DecodePeerPathControl(receive(t, controls.sent))
			if err != nil {
				t.Fatal(err)
			}
			if control.Kind == protocolsession.PeerPathDemand && control.PeerPathID == [16]byte(target) {
				return
			}
		}
	}
	waitDemand(session.direct.key.path)

	second, err := session.owner.openDirect(session.ctx, session.config)
	if err != nil {
		t.Fatal(err)
	}
	session.watchPath(second)
	waitDemand(second.key.path)

	if binds, detaches, attached := controls.counts(); binds != 1 || detaches != 0 || !attached {
		t.Fatalf("after parallel path open: binds=%d detaches=%d attached=%v", binds, detaches, attached)
	}

	sendRevoke := func(target v2signal.PeerPathID, seq uint64) {
		t.Helper()
		body, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
			PeerPathID:          [16]byte(target),
			NetworkGenerationID: [16]byte{2},
			ControlSequence:     seq,
			Kind:                protocolsession.PeerPathRevoke,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := controls.receive(body); err != nil {
			t.Fatal(err)
		}
	}
	waitDone := func(done <-chan struct{}) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-done:
				return
			case timer := <-clock.timers:
				if timer.duration == time.Second {
					timer.fire(clock.Now())
				}
			case <-deadline:
				t.Fatal("path did not finish")
			}
		}
	}
	sendRevoke(second.key.path, 1)
	waitDone(second.Done())
	if second.Result().Cause != ErrPathTerminal {
		t.Fatalf("second path result = %+v, want ErrPathTerminal", second.Result())
	}
	select {
	case <-session.Done():
		t.Fatal("retiring optional path closed session while primary remained")
	default:
	}

	sendRevoke(session.direct.key.path, 1)
	waitDone(session.direct.Done())
	if session.direct.Result().Cause != ErrPathTerminal {
		t.Fatalf("primary path result = %+v, want ErrPathTerminal", session.direct.Result())
	}
	receive(t, session.Done())

	if binds, detaches, attached := controls.counts(); binds != 1 || detaches != 1 || attached {
		t.Fatalf("after close: binds=%d detaches=%d attached=%v", binds, detaches, attached)
	}
}
