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
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type fakeNetwork struct{}

func (fakeNetwork) Snapshot(context.Context) (networkstate.State, error) {
	return networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("127.0.0.1"), InterfaceIndex: 1}}}, nil
}

type fakePathControls struct {
	mu      sync.Mutex
	handler func(context.Context, []byte) error
	sent    chan []byte
}

func (c *fakePathControls) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	c.mu.Lock()
	c.handler = handler
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
func TestAuthenticatedPathControlsWakeOnlyTheExistingOwnerAndRevokeRetires(t *testing.T) {
	clock := newClock()
	native := nativepeer.New(nativepeer.Config{Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond), Now: clock.Now, Reachability: reachability.New(reachability.Config{Now: clock.Now})})
	defer native.Close(context.Background())
	controls := &fakePathControls{sent: make(chan []byte, 32)}
	owner, _ := New(Config{Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	path := &Path{owner: owner, config: PathConfig{Native: native, Controls: controls}, key: pathKey{path: v2signal.PeerPathID{1}}, ctx: ctx, cancel: cancel, demand: ContentDemand, resourceActive: true,
		wake: make(chan struct{}, 1), resourceChanges: make(chan struct{}, 1)}
	owner.paths[path.key] = path
	done := make(chan struct{})
	go path.maintainResources(ctx, done)
	first := receive(t, controls.sent)
	control, err := protocolsession.DecodePeerPathControl(first)
	if err != nil || control.Kind != protocolsession.PeerPathDemand || control.HoldFor != nativepeer.ControlLifetime {
		t.Fatal(control, err)
	}
	receive(t, clock.timers)
	if err := controls.receive([]byte{0xff}); err != nil {
		t.Fatal(err)
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
		if kind == protocolsession.PeerPathRevoke && !path.consumeRestart() {
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
	owner, _ := New(Config{Clock: clock})
	native := nativepeer.New(nativepeer.Config{Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond), Now: clock.Now, Reachability: reachability.New(reachability.Config{Now: clock.Now})})
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
