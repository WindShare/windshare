package provider

import (
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/pion/ice/v4"
)

// Count socket claims as well as published candidates: signaling filters cannot
// prove that a disabled path was never admitted into the ICE agent.
type mappingGatherMux struct {
	priorityFixtureMux
	claims atomic.Int32
}

func (m *mappingGatherMux) GetConnForURL(_, _ string, address net.Addr) (net.PacketConn, error) {
	m.claims.Add(1)
	return newPriorityPacket(address), nil
}

func (m *mappingGatherMux) GetConnForEndpoint(_ string, address netip.AddrPort) (net.PacketConn, error) {
	m.claims.Add(1)
	return newPriorityPacket(net.TCPAddrFromAddrPort(address)), nil
}

func TestMappedGatheringRespectsCandidateAndNetworkTypes(t *testing.T) {
	mappings := []ice.MappedEndpoint{
		{Local: netip.MustParseAddrPort("192.0.2.1:4000"), External: netip.MustParseAddrPort("203.0.113.1:5000")},
		{Local: netip.MustParseAddrPort("[2001:db8::1]:4000"), External: netip.MustParseAddrPort("[2001:db8:ffff::1]:5000")},
	}
	allNetworks := []ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6, ice.NetworkTypeTCP4, ice.NetworkTypeTCP6}
	policies := []struct {
		name    string
		types   []ice.CandidateType
		allowed bool
	}{
		{name: "default", allowed: true},
		{name: "srflx-only", types: []ice.CandidateType{ice.CandidateTypeServerReflexive}, allowed: true},
		{name: "host-only", types: []ice.CandidateType{ice.CandidateTypeHost}},
		{name: "relay-only", types: []ice.CandidateType{ice.CandidateTypeRelay}},
		{name: "host-and-relay", types: []ice.CandidateType{ice.CandidateTypeHost, ice.CandidateTypeRelay}},
	}
	networks := []struct {
		name  string
		types []ice.NetworkType
	}{
		{name: "dual-stack-udp-and-tcp", types: allNetworks},
		{name: "udp4-only", types: []ice.NetworkType{ice.NetworkTypeUDP4}},
		{name: "udp6-only", types: []ice.NetworkType{ice.NetworkTypeUDP6}},
		{name: "tcp4-only", types: []ice.NetworkType{ice.NetworkTypeTCP4}},
		{name: "tcp6-only", types: []ice.NetworkType{ice.NetworkTypeTCP6}},
	}
	for _, policy := range policies {
		for _, network := range networks {
			t.Run(policy.name+"/"+network.name, func(t *testing.T) {
				mux := &mappingGatherMux{}
				options := []ice.AgentOption{
					ice.WithNetworkTypes(network.types),
					ice.WithMulticastDNSMode(ice.MulticastDNSModeDisabled),
					// The supplied mappings are frozen socket capabilities. Exclude
					// machine-dependent host discovery and use no STUN/TURN server.
					ice.WithInterfaceFilter(func(string) bool { return false }),
					ice.WithProviderConfig(ice.ProviderConfig{
						SrflxMux: mux, TCPMappedMux: mux,
						MappedUDPEndpoints: mappings, MappedTCPEndpoints: mappings,
					}),
				}
				if policy.types != nil {
					options = append(options, ice.WithCandidateTypes(policy.types))
				}
				agent, err := ice.NewAgentWithOptions(options...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = agent.Close() })
				published := collectMappedGathering(t, agent, len(allNetworks))
				wantCount := 0
				if policy.allowed {
					wantCount = len(network.types)
				}
				if len(published) != wantCount || int(mux.claims.Load()) != wantCount {
					t.Fatalf("published=%d, socket claims=%d, want=%d", len(published), mux.claims.Load(), wantCount)
				}
				seen := make(map[ice.NetworkType]bool)
				for _, candidate := range published {
					kind := candidate.NetworkType()
					if !slices.Contains(network.types, kind) || seen[kind] || candidate.Type() != ice.CandidateTypeServerReflexive {
						t.Fatalf("unexpected or duplicate mapped candidate: %s", candidate)
					}
					seen[kind] = true
					endpoint := mappings[0]
					if kind.IsIPv6() {
						endpoint = mappings[1]
					}
					related := candidate.RelatedAddress()
					if candidate.Address() != endpoint.External.Addr().String() || candidate.Port() != int(endpoint.External.Port()) ||
						related == nil || related.Address != endpoint.Local.Addr().String() || related.Port != int(endpoint.Local.Port()) {
						t.Fatalf("mapping lost its external endpoint or real base: %s", candidate)
					}
					if kind.IsTCP() && candidate.TCPType() != ice.TCPTypePassive {
						t.Fatalf("mapped TCP direction lost: %s", candidate)
					}
				}
				local, err := agent.GetLocalCandidates()
				if err != nil {
					t.Fatal(err)
				}
				if len(local) != wantCount || len(agent.GetLocalCandidatesStats()) != wantCount {
					t.Fatalf("internal candidate set differs from policy: %v", local)
				}
				for _, candidate := range local {
					if !slices.ContainsFunc(published, func(p ice.Candidate) bool { return p.Marshal() == candidate.Marshal() }) {
						t.Fatalf("unpublished internal candidate: %s", candidate)
					}
				}
			})
		}
	}
}

func collectMappedGathering(t *testing.T, agent *ice.Agent, capacity int) []ice.Candidate {
	t.Helper()
	published := make(chan ice.Candidate, capacity+1)
	if err := agent.OnCandidate(func(candidate ice.Candidate) { published <- candidate }); err != nil {
		t.Fatal(err)
	}
	if err := agent.GatherCandidates(); err != nil {
		t.Fatal(err)
	}
	var candidates []ice.Candidate
	for {
		candidate := await(t, published)
		if candidate == nil {
			return candidates
		}
		candidates = append(candidates, candidate)
	}
}
