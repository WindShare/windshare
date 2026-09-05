package nativepeer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type admissionTestTimer struct {
	at      time.Time
	run     func()
	stopped bool
}
type admissionTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*admissionTestTimer
}

func (c *admissionTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *admissionTestClock) AfterFunc(d time.Duration, f func()) func() {
	c.mu.Lock()
	timer := &admissionTestTimer{at: c.now.Add(d), run: f}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	return func() { c.mu.Lock(); timer.stopped = true; c.mu.Unlock() }
}
func (c *admissionTestClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var run func()
		for _, timer := range c.timers {
			if !timer.stopped && !timer.at.After(c.now) {
				timer.stopped = true
				run = timer.run
				break
			}
		}
		c.mu.Unlock()
		if run == nil {
			return
		}
		run()
	}
}
func newAdmissionFixture(t *testing.T, endpoints int) (*ProcessAdmission, *admissionTestClock, func() *NativePeerConnectivity) {
	t.Helper()
	clock := &admissionTestClock{now: time.Unix(100, 0)}
	gate := NewProcessAdmission(AdmissionClock{Now: clock.Now, AfterFunc: clock.AfterFunc})
	var entries []icepolicy.Endpoint
	for i := range endpoints {
		entries = append(entries, icepolicy.Endpoint{ID: fmt.Sprint(i), URL: fmt.Sprintf("stun:127.0.0.1:%d", 30000+i), Family: "any", Trust: "local", Enabled: true, FailureDomain: fmt.Sprint(i)})
	}
	pool, err := icepolicy.NewICEEndpointPool(entries)
	if err != nil {
		t.Fatal(err)
	}
	makeNative := func() *NativePeerConnectivity {
		monitor := &testMonitor{state: networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("127.0.0.1"), InterfaceIndex: 1}}}}
		n := New(Config{Admission: gate, Now: clock.Now, Monitor: monitor, Pool: &pool, Reachability: reachability.New(reachability.Config{Now: clock.Now}), ObservationCapacity: 256})
		t.Cleanup(func() {
			if err := n.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		return n
	}
	return gate, clock, makeNative
}
func attemptFor(session byte) AttemptRequest {
	r := request(1)
	r.ProtocolSessionID = [16]byte{session}
	return r
}
func startActual(t *testing.T, n *NativePeerConnectivity, session byte) *provider.Connection {
	t.Helper()
	pc, err := n.NewPeerConnection(context.Background(), attemptFor(session))
	if err != nil {
		t.Fatal(err)
	}
	return pc
}
func admissionEvent(t *testing.T, n *NativePeerConnectivity, kind AdmissionKind, session byte) AdmissionFacts {
	t.Helper()
	for {
		select {
		case event := <-n.Observations():
			if event.Admission != nil && event.Admission.Kind == kind && event.Subject.ProtocolSessionID == [16]byte{session} {
				return *event.Admission
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing %s for %d", kind, session)
			return AdmissionFacts{}
		}
	}
}

type admissionResult struct {
	peer *provider.Connection
	err  error
}

func queuedActual(t *testing.T, n *NativePeerConnectivity, session byte, ctx context.Context) <-chan admissionResult {
	t.Helper()
	done := make(chan admissionResult, 1)
	go func() { pc, err := n.NewPeerConnection(ctx, attemptFor(session)); done <- admissionResult{pc, err} }()
	admissionEvent(t, n, AdmissionQueued, session)
	return done
}
func actualResult(t *testing.T, ch <-chan admissionResult) admissionResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not return")
		return admissionResult{}
	}
}
func TestProcessProviderAdmissionFairAcrossManagersAndIndependentCancellation(t *testing.T) {
	gate, _, makeNative := newAdmissionFixture(t, 0)
	a, b, c := makeNative(), makeNative(), makeNative()
	var active []*provider.Connection
	for i := byte(1); i <= 4; i++ {
		active = append(active, startActual(t, a, i))
	}
	a1 := queuedActual(t, a, 5, context.Background())
	a2 := queuedActual(t, a, 6, context.Background())
	b1 := queuedActual(t, b, 7, context.Background())
	c1 := queuedActual(t, c, 8, context.Background())
	canceled, cancel := context.WithCancel(context.Background())
	dropped := queuedActual(t, b, 9, canceled)
	cancel()
	if !errors.Is(actualResult(t, dropped).err, context.Canceled) {
		t.Fatal("local cancellation lost")
	}
	if err := active[0].Close(); err != nil {
		t.Fatal(err)
	}
	first := actualResult(t, a1)
	if first.err != nil {
		t.Fatal(first.err)
	}
	if err := first.peer.Close(); err != nil {
		t.Fatal(err)
	}
	second := actualResult(t, b1)
	if second.err != nil {
		t.Fatal(second.err)
	}
	if err := second.peer.Close(); err != nil {
		t.Fatal(err)
	}
	third := actualResult(t, c1)
	if third.err != nil {
		t.Fatal(third.err)
	}
	if err := third.peer.Close(); err != nil {
		t.Fatal(err)
	}
	fourth := actualResult(t, a2)
	if fourth.err != nil {
		t.Fatal(fourth.err)
	}
	a.SetDirect(attemptFor(6).ProtocolSessionID, attemptFor(6).Binding.PeerPathID)
	a.SetDirect(attemptFor(6).ProtocolSessionID, attemptFor(6).Binding.PeerPathID)
	gate.mu.Lock()
	count := gate.active
	queued := len(gate.queue)
	gate.mu.Unlock()
	if count != 3 || queued != 0 {
		t.Fatal(count, queued)
	}
	// Admission releases attempt capacity while the accepted physical connection lives.
	if fourth.peer.ConnectionState().String() == "closed" {
		t.Fatal("admission closed provider")
	}
	if err := fourth.peer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pc := range active {
		_ = pc.Close()
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 {
		t.Fatal("double detach retained capacity", gate.active)
	}
}
func TestProcessProviderChurnCannotResetStartOrSTUNBudget(t *testing.T) {
	gate, clock, makeNative := newAdmissionFixture(t, 2)
	for i := byte(1); i <= ProcessStartsPerWindow; i++ {
		n := makeNative()
		pc := startActual(t, n, i)
		if err := pc.Close(); err != nil {
			t.Fatal(err)
		}
		n.CloseSession(attemptFor(i).ProtocolSessionID)
	}
	n := makeNative()
	pending := queuedActual(t, n, 40, context.Background())
	gate.mu.Lock()
	starts, endpoints, active := gate.starts, gate.endpoints, gate.active
	gate.mu.Unlock()
	if starts != 0 || endpoints != 0 || active != 0 {
		t.Fatal(starts, endpoints, active)
	}
	clock.advance(ProcessAdmissionWindow/ProcessStartsPerWindow - time.Nanosecond)
	select {
	case <-pending:
		t.Fatal("rate refilled early")
	default:
	}
	clock.advance(time.Nanosecond)
	result := actualResult(t, pending)
	if result.err != nil {
		t.Fatal(result.err)
	}
	_ = result.peer.Close()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.starts != 0 || gate.endpoints != 0 {
		t.Fatal("fresh manager minted allowance")
	}
}
func TestProcessQueueCloseAndActiveDeadlineReleaseRealProvider(t *testing.T) {
	gate, clock, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	for i := byte(1); i <= 4; i++ {
		startActual(t, n, i)
	}
	waiting := queuedActual(t, n, 5, context.Background())
	n.CloseSession(attemptFor(5).ProtocolSessionID)
	if actualResult(t, waiting).err == nil {
		t.Fatal("closed session reached provider")
	}
	clock.advance(ProcessAttemptBudget)
	released := make(map[[16]byte]bool)
	for len(released) < 4 {
		select {
		case event := <-n.Observations():
			if event.Admission != nil && event.Admission.Kind == AdmissionReleased {
				released[event.Subject.ProtocolSessionID] = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("active deadline did not detach", released)
		}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 || len(gate.queue) != 0 {
		t.Fatal("expired work retained admission")
	}
}
func TestPreparationAbandonAndStaleGenerationNeverCreateProvider(t *testing.T) {
	gate, _, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	prepared, err := n.PrepareAttempt(context.Background(), attemptFor(1))
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.generation++
	n.mu.Unlock()
	if _, err = prepared.Start(context.Background()); !errors.Is(err, ErrProcessAdmission) {
		t.Fatal(err)
	}
	prepared.Close()
	prepared.Close()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 {
		t.Fatal("stale generation retained permit")
	}
}
