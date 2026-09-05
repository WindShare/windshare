package nativepeer

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

func TestSamePathInDifferentSessionsHasIndependentSocketOwnership(t *testing.T) {
	h := newHarness(t)
	first := request(1)
	second := first
	second.ProtocolSessionID = [16]byte{9}
	_, _ = h.native.NewPeerConnection(context.Background(), first)
	_, _ = h.native.NewPeerConnection(context.Background(), second)
	a, b := h.requests[0].SocketLease, h.requests[1].SocketLease
	if a == b || a.SessionID() == b.SessionID() || a.Endpoints()[0] == b.Endpoints()[0] {
		t.Fatal("sessions aliased a physical socket")
	}
	h.native.CloseSession(first.ProtocolSessionID)
	retained, err := b.Retain()
	if err != nil {
		t.Fatal("closing one session retired its peer", err)
	}
	_ = retained.Close()
	h.native.CloseSession(second.ProtocolSessionID)
}
func TestAddressAllocationKeepsFamiliesAndInterfacesWithinSocketLimit(t *testing.T) {
	var addresses []networkstate.Address
	for i := 1; i <= 30; i++ {
		addresses = append(addresses, networkstate.Address{InterfaceIndex: 1, IP: netip.MustParseAddr(fmt.Sprintf("10.0.0.%d", i))})
	}
	ipv6 := netip.MustParseAddr("2001:4860::1")
	other := netip.MustParseAddr("192.168.1.2")
	addresses = append(addresses, networkstate.Address{InterfaceIndex: 1, IP: ipv6}, networkstate.Address{InterfaceIndex: 2, IP: other}, networkstate.Address{})
	chosen := selectAddresses(addresses)
	if len(chosen) != socketauthority.MaxAddressesPerPath || !slices.Contains(chosen, ipv6) || !slices.Contains(chosen, other) {
		t.Fatal(chosen)
	}
	if len(selectAddresses(nil)) != 0 {
		t.Fatal("empty allocation")
	}
	if len(selectAddresses([]networkstate.Address{{IP: netip.MustParseAddr("127.0.0.1")}})) != 1 {
		t.Fatal("loopback fixture unsupported")
	}
}
func TestProfilesRotateOnceAndNeverLoseSingletonSTUNOnLaterAttempts(t *testing.T) {
	for _, count := range []int{1, 4} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			h := newHarness(t)
			var endpoints []icepolicy.Endpoint
			for i := 0; i < count; i++ {
				id := fmt.Sprint(i)
				endpoints = append(endpoints, icepolicy.Endpoint{ID: id, URL: "stun:stun" + id + ".example:3478", Family: "any", Trust: "local", Enabled: true, FailureDomain: id})
			}
			pool, err := icepolicy.NewICEEndpointPool(endpoints)
			if err != nil {
				t.Fatal(err)
			}
			h.native.config.Pool = &pool
			for i := uint64(1); i <= 3; i++ {
				_, _ = h.native.NewPeerConnection(context.Background(), request(i))
			}
			a, b, c := h.requests[0], h.requests[1], h.requests[2]
			if len(a.STUNURLs) == 0 || len(b.STUNURLs) == 0 || !slices.Equal(b.STUNURLs, c.STUNURLs) || b.ICEProfileID != c.ICEProfileID {
				t.Fatal(a.STUNURLs, b.STUNURLs, c.STUNURLs)
			}
			if count == 1 && !slices.Equal(a.STUNURLs, b.STUNURLs) {
				t.Fatal("singleton was consumed")
			}
			if count == 4 && (len(a.STUNURLs) != 2 || len(b.STUNURLs) != 2 || slices.Contains(a.STUNURLs, b.STUNURLs[0])) {
				t.Fatal("backup did not explore unused domains")
			}
			h.monitor.state.ResumeSequence++
			_, _ = h.native.NewPeerConnection(context.Background(), request(4))
			replacement := h.requests[3]
			if replacement.NetworkGenerationID == c.NetworkGenerationID || replacement.ICEProfileID == c.ICEProfileID || !slices.Equal(replacement.STUNURLs, c.STUNURLs) {
				t.Fatal("network replacement reset wave footprint", replacement.STUNURLs, c.STUNURLs)
			}
			h.native.BeginWave(a.ProtocolSessionID, request(1).Binding.PeerPathID)
			_, _ = h.native.NewPeerConnection(context.Background(), request(5))
			if len(h.requests[4].STUNURLs) == 0 {
				t.Fatal("new wave could not select")
			}
		})
	}
}
func TestMappedCandidateCannotClaimSTUNSuccessOrHostDelay(t *testing.T) {
	h := newHarness(t)
	_, _ = h.native.NewPeerConnection(context.Background(), request(1))
	emit := h.requests[0].Observe
	emit(provider.Event{At: h.now, Candidate: &provider.CandidateFacts{Type: "host", Origin: "ordinary"}})
	emit(provider.Event{At: h.now.Add(time.Second), Candidate: &provider.CandidateFacts{Type: "srflx", Origin: "mapped"}})
	emit(provider.Event{At: h.now.Add(2 * time.Second), Milestone: "gathering_complete"})
	facts := h.native.facts.Snapshot()
	if len(facts.Profiles) != 1 || facts.Profiles[0].ServerReflexiveProduced || facts.Profiles[0].FirstCandidateDelay != 0 || len(facts.Endpoints) != 0 {
		t.Fatal(facts)
	}
}
func TestObservationQueueFreezesProviderFactsAndReportsExactLossCut(t *testing.T) {
	n := New(Config{ObservationCapacity: 1, Side: SideReceiver})
	defer n.Close(context.Background())
	subject := Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, AttemptID: [16]byte{3}, AttemptSequence: 4, NetworkGenerationID: 5, ICEProfileID: "profile", Side: SideReceiver}
	candidate := &provider.CandidateFacts{Type: "host"}
	pair := &provider.PairFacts{LocalAddress: "192.0.2.1"}
	n.observeProvider(subject, provider.Event{Candidate: candidate, Pair: pair})
	candidate.Type = "mutated"
	pair.LocalAddress = "mutated"
	n.observeReachability(reachability.Event{Endpoint: reachability.Endpoint{Generation: 9}})
	cut := n.CompleteObservations()
	if cut.Enqueued != 1 || cut.CapacityDropped != 1 {
		t.Fatal(cut)
	}
	observation := <-n.Observations()
	if observation.Subject != subject || observation.Provider.Candidate.Type != "host" || observation.Provider.Pair.LocalAddress != "192.0.2.1" {
		t.Fatal(observation)
	}
	if _, ok := <-n.Observations(); ok {
		t.Fatal("source not completed")
	}
	if n.CompleteObservations() != cut {
		t.Fatal("completion changed")
	}
	var absent *NativePeerConnectivity
	if absent.Observations() != nil || absent.CompleteObservations().Enqueued != 0 {
		t.Fatal("absent source")
	}
}
func TestPrewarmIsSessionOwnedUntilSessionClose(t *testing.T) {
	h := newHarness(t)
	session := [16]byte{1}
	if !h.native.ClaimPrewarm(session) || h.native.ClaimPrewarm(session) {
		t.Fatal("prewarm repeated")
	}
	h.native.CloseSession(session)
	if !h.native.ClaimPrewarm([16]byte{2}) {
		t.Fatal("new authenticated session denied")
	}
}
