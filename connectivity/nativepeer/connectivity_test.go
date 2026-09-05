package nativepeer

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type testMonitor struct {
	observer networkstate.Observer
	state    networkstate.State
	err      error
}

func (m *testMonitor) Poll(_ context.Context, now time.Time) (networkstate.Snapshot, bool, error) {
	if m.err != nil {
		return networkstate.Snapshot{}, false, m.err
	}
	snapshot, changed := m.observer.Observe(m.state, now)
	return snapshot, changed, nil
}

type harness struct {
	native   *NativePeerConnectivity
	monitor  *testMonitor
	now      time.Time
	requests []provider.AttemptConfig
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{now: time.Unix(100, 0), monitor: &testMonitor{state: networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("127.0.0.1"), InterfaceIndex: 1}}}}}
	pool, _ := icepolicy.NewICEEndpointPool(nil)
	sockets := socketauthority.New(socketauthority.Config{})
	h.native = New(Config{Admission: NewProcessAdmission(AdmissionClock{}), Monitor: h.monitor, Sockets: sockets, Pool: &pool, Now: func() time.Time { return h.now },
		Reachability: reachability.New(reachability.Config{Now: func() time.Time { return h.now }}),
		Connect: func(_ pion.Configuration, request provider.AttemptConfig) (*provider.Connection, error) {
			h.requests = append(h.requests, request)
			return nil, errors.New("capture")
		},
	})
	t.Cleanup(func() { h.native.CloseSession([16]byte{1}); _ = sockets.Close() })
	return h
}
func request(sequence uint64) AttemptRequest {
	return AttemptRequest{ProtocolSessionID: [16]byte{1}, Binding: v2signal.Binding{PeerPathID: v2signal.PeerPathID{2}, AttemptID: v2signal.AttemptID{byte(sequence)}, AttemptSequence: sequence}}
}
func TestFreshAttemptsFreezeProfileAndReuseExactSocketWithinGeneration(t *testing.T) {
	h := newHarness(t)
	first := request(1)
	if _, err := h.native.NewPeerConnection(context.Background(), first); err == nil {
		t.Fatal("capture error lost")
	}
	_, _ = h.native.NewPeerConnection(context.Background(), request(2))
	a, b := h.requests[0], h.requests[1]
	if a.SocketLease != b.SocketLease || a.SocketLease.Endpoints()[0] != b.SocketLease.Endpoints()[0] || a.AttemptID == b.AttemptID || a.InitialCheckingTimeout < 40*time.Second {
		t.Fatal(a, b)
	}
	if a.ICEProfileID == "" || a.ProtocolSessionID != first.ProtocolSessionID || a.NetworkGenerationID != 1 || len(a.STUNURLs) != 0 {
		t.Fatal(a)
	}
	changes, _ := h.native.SetDemand(first.ProtocolSessionID, first.Binding.PeerPathID, true, false)
	h.monitor.state.ResumeSequence++
	h.native.Maintain(context.Background())
	select {
	case change := <-changes:
		if change.NetworkGenerationID != 2 {
			t.Fatal(change)
		}
	default:
		t.Fatal("missing generation replacement")
	}
	_, _ = h.native.NewPeerConnection(context.Background(), request(3))
	c := h.requests[2]
	if c.SocketLease == a.SocketLease || c.NetworkGenerationID != 2 {
		t.Fatal("old socket snapshot revived")
	}
	if _, err := a.SocketLease.Retain(); err == nil {
		t.Fatal("retired socket could be retained")
	}
	h.native.ClosePath(first.ProtocolSessionID, first.Binding.PeerPathID)
	if h.native.Generation(first.ProtocolSessionID, first.Binding.PeerPathID) != 0 {
		t.Fatal("path retained")
	}
}
func controlBytes(t *testing.T, sequence uint64, kind protocolsession.PeerPathControlKind, hold time.Duration) []byte {
	t.Helper()
	control := protocolsession.PeerPathControl{PeerPathID: [16]byte{2}, NetworkGenerationID: [16]byte{3}, ControlSequence: sequence, Kind: kind, ProviderProfile: string(provider.TCPNativeWindows)}
	if kind != protocolsession.PeerPathRevoke {
		control.ValidFor = ControlLifetime
		control.HoldFor = hold
	}
	body, err := protocolsession.EncodePeerPathControl(control)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func TestAuthenticatedDemandExpiresAndWatermarkPreventsResurrection(t *testing.T) {
	h := newHarness(t)
	session := [16]byte{1}
	key := pathKey{session, v2signal.PeerPathID{2}}
	demand := controlBytes(t, 1, protocolsession.PeerPathDemand, ControlLifetime)
	if _, ok := h.native.ApplyControl(session, demand); !ok {
		t.Fatal("demand rejected")
	}
	_, _ = h.native.NewPeerConnection(context.Background(), request(1))
	if !h.native.paths[key].content || h.requests[0].TCPProfile != provider.TCPNativeWindows {
		t.Fatal("authenticated capability not frozen")
	}
	h.now = h.now.Add(ControlLifetime)
	h.native.Maintain(context.Background())
	if h.native.paths[key].content {
		t.Fatal("expired demand renewed mapping")
	}
	if _, ok := h.native.ApplyControl(session, demand); ok {
		t.Fatal("expired replay revived demand")
	}
	if _, ok := h.native.ApplyControl(session, controlBytes(t, 2, protocolsession.PeerPathRevoke, 0)); !ok {
		t.Fatal("revoke rejected")
	}
	notify(h.native.paths[key], Change{NetworkGenerationID: 3})
	if change := <-h.native.paths[key].changes; !change.Retired {
		t.Fatal("network change replaced terminal retirement", change)
	}
	if _, ok := h.native.ApplyControl(session, controlBytes(t, 3, protocolsession.PeerPathMappingReady, 0)); ok {
		t.Fatal("late readiness revived revoked path")
	}
	if h.native.paths[key].content {
		t.Fatal("late mapping revived demand")
	}
	if _, ok := h.native.ApplyControl(session, []byte{0xff}); ok {
		t.Fatal("malformed control accepted")
	}
	body, err := h.native.Control(session, v2signal.PeerPathID{4}, protocolsession.PeerPathDemand, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocolsession.DecodePeerPathControl(body)
	if err != nil || decoded.ControlSequence != 1 || decoded.HoldFor != ControlLifetime || decoded.ProviderProfile != string(provider.LocalTCPProfile()) {
		t.Fatal(decoded, err)
	}
}
func TestNativeDemandAdmissionBoundsAndMonitorFailure(t *testing.T) {
	h := newHarness(t)
	if _, err := h.native.SetDemand([16]byte{1}, v2signal.PeerPathID{}, true, false); err == nil {
		t.Fatal("zero path")
	}
	for i := 1; i <= MaximumPaths; i++ {
		_, err := h.native.SetDemand([16]byte{1}, v2signal.PeerPathID{byte(i)}, false, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.native.SetDemand([16]byte{1}, v2signal.PeerPathID{255}, true, false); err == nil {
		t.Fatal("capacity")
	}
	h.native.CloseSession([16]byte{1})
	if len(h.native.paths) != 0 {
		t.Fatal("session retained")
	}
	h.monitor.err = errors.New("snapshot unavailable")
	if _, err := h.native.NewPeerConnection(context.Background(), request(1)); err == nil {
		t.Fatal("monitor error swallowed")
	}
	if _, err := h.native.NewPeerConnection(nil, request(1)); err == nil {
		t.Fatal("nil context")
	}
}
func TestProfileFactsComeFromProviderCandidatesWithoutInventedEndpointSuccess(t *testing.T) {
	h := newHarness(t)
	_, _ = h.native.NewPeerConnection(context.Background(), request(1))
	emit := h.requests[0].Observe
	emit(provider.Event{At: h.now.Add(time.Second), Candidate: &provider.CandidateFacts{Type: "host"}})
	emit(provider.Event{At: h.now.Add(2 * time.Second), Candidate: &provider.CandidateFacts{Type: "srflx", Origin: "ordinary"}})
	facts := h.native.facts.Snapshot()
	if len(facts.Endpoints) != 0 || len(facts.Profiles) != 1 || !facts.Profiles[0].ServerReflexiveProduced || facts.Profiles[0].FirstCandidateDelay != 2*time.Second {
		t.Fatal(facts)
	}
}
