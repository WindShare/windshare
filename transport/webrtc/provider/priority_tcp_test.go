package provider

import (
	"net"
	"net/netip"
	"testing"

	"github.com/pion/ice/v4"
)

func TestPriorityDynamicActiveTCPUsesTheSameAdmissionPolicy(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: listener})
	t.Cleanup(func() { _ = mux.Close() })
	const tcpOffset = 5
	agent, err := ice.NewAgentWithOptions(
		ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeTCP4}),
		ice.WithCandidateTypes([]ice.CandidateType{ice.CandidateTypeHost}),
		ice.WithIncludeLoopback(),
		ice.WithIPFilter(func(ip net.IP) bool { return ip.Equal(net.IPv4(127, 0, 0, 1)) }),
		ice.WithMulticastDNSMode(ice.MulticastDNSModeDisabled),
		ice.WithTCPMux(mux), ice.WithTCPPriorityOffset(tcpOffset),
		ice.WithProviderConfig(ice.ProviderConfig{LocalAddressOrder: []netip.Addr{
			netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("127.0.0.1"),
		}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	published := make(chan ice.Candidate, 8)
	if err = agent.OnCandidate(func(candidate ice.Candidate) {
		if candidate != nil {
			copy, parseErr := ice.UnmarshalCandidate(candidate.Marshal())
			if parseErr != nil {
				panic(parseErr)
			}
			published <- copy
		} else {
			published <- nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err = agent.GatherCandidates(); err != nil {
		t.Fatal(err)
	}
	passive := await(t, published)
	if passive == nil || passive.TCPType() != ice.TCPTypePassive {
		t.Fatalf("passive candidate missing: %v", passive)
	}
	if candidate := await(t, published); candidate != nil {
		t.Fatalf("unexpected initial candidate: %s", candidate)
	}
	candidates := []ice.Candidate{passive}

	for range 2 {
		remoteListener, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		t.Cleanup(func() { _ = remoteListener.Close() })
		remote, candidateErr := ice.NewCandidateHost(&ice.CandidateHostConfig{
			Network: "tcp4", Address: "127.0.0.1", Port: remoteListener.Addr().(*net.TCPAddr).Port,
			Component: ice.ComponentRTP, TCPType: ice.TCPTypePassive,
		})
		if candidateErr != nil {
			t.Fatal(candidateErr)
		}
		if err = agent.AddRemoteCandidate(remote); err != nil {
			t.Fatal(err)
		}
		active := await(t, published)
		if active == nil || active.TCPType() != ice.TCPTypeActive {
			t.Fatalf("active candidate missing: %v", active)
		}
		candidates = append(candidates, active)
	}
	typePreference := uint32(ice.CandidateTypeHost.Preference() - tcpOffset)
	// The first active and passive use base rank 1. The second active uses
	// the secondary pool, leaving rank 0 free for a late interface.
	for index, expectedLocal := range []uint32{(4 << 13) + 8190, (6 << 13) + 8190, (6 << 13) + 8189} {
		want := typePreference<<24 | expectedLocal<<8 | 255
		if candidates[index].Priority() != want {
			t.Fatalf("candidate %s priority=%d, want %d", candidates[index], candidates[index].Priority(), want)
		}
	}
	assertPriorityPublication(t, agent, candidates)
	// Pairs must reference admitted candidates, including both dynamic actives.
	local, err := agent.GetLocalCandidates()
	if err != nil {
		t.Fatal(err)
	}
	paired := make(map[string]bool)
	for _, pair := range agent.GetCandidatePairsStats() {
		paired[pair.LocalCandidateID] = true
	}
	for _, candidate := range local {
		if candidate.TCPType() == ice.TCPTypeActive && !paired[candidate.ID()] {
			t.Fatalf("dynamic active candidate absent from checklist: %s", candidate)
		}
	}
}
