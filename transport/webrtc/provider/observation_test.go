package provider

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

func TestCandidateFactsPreserveMappedProvenanceAndActualMilestones(t *testing.T) {
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	lease := testLease(t, authority, 1, "127.0.0.1")
	local := lease.Endpoints()[0]
	external := netip.MustParseAddrPort("203.0.113.1:45678")
	var mu sync.Mutex
	var events []Event
	request := AttemptConfig{ProtocolSessionID: lease.SessionID(), NetworkGenerationID: 1, PeerPathID: lease.PathID(), SocketLease: lease, MappedEndpoints: []MappedEndpoint{{Local: local, External: external}},
		Observe: func(event Event) { mu.Lock(); events = append(events, event); mu.Unlock() }}
	connection, err := NewPeerConnection(pion.Configuration{}, request)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	// Mutating the caller's slice cannot inject a late local candidate.
	request.MappedEndpoints[0].External = netip.MustParseAddrPort("203.0.113.2:45679")
	if _, err = connection.CreateDataChannel("observe", nil); err != nil {
		t.Fatal(err)
	}
	complete := make(chan struct{})
	var advertised string
	connection.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate == nil {
			close(complete)
		} else if candidate.Port == external.Port() {
			advertised = candidate.ToJSON().Candidate
		}
	})
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = connection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	await(t, complete)
	if !connection.IsMappedCandidate(advertised) {
		t.Fatal("frozen mapping not recognized")
	}
	if connection.IsMappedCandidate("garbage") {
		t.Fatal("malformed candidate has provenance")
	}
	ordinary := "candidate:1 1 udp 1 203.0.113.1 45678 typ srflx raddr 127.0.0.2 rport 1"
	if connection.IsMappedCandidate(ordinary) {
		t.Fatal("srflx syntax acquired mapping provenance")
	}
	connection.observeSelectedPair(&pion.ICECandidatePair{Local: &pion.ICECandidate{Protocol: pion.ICEProtocolUDP, Typ: pion.ICECandidateTypeHost, Address: "127.0.0.1", Port: local.Port()}, Remote: &pion.ICECandidate{Protocol: pion.ICEProtocolUDP, Typ: pion.ICECandidateTypeSrflx, Address: "203.0.113.1", Port: 45678}})
	_ = connection.Close()
	mu.Lock()
	defer mu.Unlock()
	sawMapped, sawHost, sawPair, sawClosed := false, false, false, false
	for _, event := range events {
		if event.PeerPathID != lease.PathID() || event.NetworkGenerationID != 1 || event.At.IsZero() {
			t.Fatal("lost provider identity")
		}
		if event.Candidate != nil {
			if event.Candidate.Origin == "mapped" {
				sawMapped = true
			} else if event.Candidate.Type == "host" {
				sawHost = true
			}
		}
		if event.Pair != nil {
			sawPair = true
		}
		if event.Milestone == "provider_closed" {
			sawClosed = true
		}
	}
	if !sawMapped || !sawHost || !sawPair || !sawClosed {
		t.Fatalf("incomplete actual provider facts: %+v", events)
	}
}

func TestOptionalTCPFailureDoesNotPreventUDPAttempt(t *testing.T) {
	authority := socketauthority.New(socketauthority.Config{Capacity: 1})
	defer authority.Close()
	lease := testLease(t, authority, 1, "127.0.0.1")
	connection, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{ProtocolSessionID: lease.SessionID(), NetworkGenerationID: 1, PeerPathID: lease.PathID(), SocketLease: lease, TCPProfile: TCPNativeWindows})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if len(lease.TCPEndpoints()) != 0 {
		t.Fatal("capacity failure created listener")
	}
}
func TestInitialBudgetAndMappingProtocolValidation(t *testing.T) {
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	lease := testLease(t, authority, 1, "127.0.0.1")
	request := AttemptConfig{ProtocolSessionID: lease.SessionID(), NetworkGenerationID: 1, PeerPathID: lease.PathID(), SocketLease: lease, InitialCheckingTimeout: time.Second}
	if _, err := NewPeerConnection(pion.Configuration{}, request); err == nil {
		t.Fatal("premature PAC ceiling accepted")
	}
	request.InitialCheckingTimeout = 40 * time.Second
	request.MappedEndpoints = []MappedEndpoint{{Local: lease.Endpoints()[0], External: netip.MustParseAddrPort("203.0.113.1:1"), Protocol: "unknown"}}
	if _, err := NewPeerConnection(pion.Configuration{}, request); err == nil {
		t.Fatal("unknown mapped protocol accepted")
	}
	request.MappedEndpoints[0].Protocol = "tcp"
	connection, err := NewPeerConnection(pion.Configuration{}, request)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if len(lease.TCPEndpoints()) != 0 {
		t.Fatal("unknown peer profile enabled TCP")
	}
}
