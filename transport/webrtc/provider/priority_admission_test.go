package provider

import (
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
)

// The fixture supplies addresses without opening interfaces or performing
// handshakes. Only real ICE gathering, admission and publication are exercised.
type priorityFixtureMux struct {
	ice.UniversalUDPMux
}

func (priorityFixtureMux) RemoveConnByUfrag(string) {}
func (priorityFixtureMux) GetConnForURL(_, _ string, address net.Addr) (net.PacketConn, error) {
	return newPriorityPacket(address), nil
}
func (priorityFixtureMux) GetConnForEndpoint(_ string, address netip.AddrPort) (net.PacketConn, error) {
	return newPriorityPacket(net.TCPAddrFromAddrPort(address)), nil
}

type priorityPacket struct {
	address net.Addr
	closed  chan struct{}
	once    sync.Once
}

func newPriorityPacket(address net.Addr) *priorityPacket {
	return &priorityPacket{address: address, closed: make(chan struct{})}
}
func (p *priorityPacket) ReadFrom([]byte) (int, net.Addr, error) {
	<-p.closed
	return 0, nil, io.EOF
}
func (*priorityPacket) WriteTo(raw []byte, _ net.Addr) (int, error) { return len(raw), nil }
func (p *priorityPacket) Close() error                              { p.once.Do(func() { close(p.closed) }); return nil }
func (p *priorityPacket) LocalAddr() net.Addr                       { return p.address }
func (*priorityPacket) SetDeadline(time.Time) error                 { return nil }
func (*priorityPacket) SetReadDeadline(time.Time) error             { return nil }
func (*priorityPacket) SetWriteDeadline(time.Time) error            { return nil }

func gatherPriorityCandidates(t *testing.T, config ice.ProviderConfig, options ...ice.AgentOption) (*ice.Agent, []ice.Candidate) {
	t.Helper()
	options = append(options,
		ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6, ice.NetworkTypeTCP4, ice.NetworkTypeTCP6}),
		ice.WithCandidateTypes([]ice.CandidateType{ice.CandidateTypeServerReflexive}),
		ice.WithMulticastDNSMode(ice.MulticastDNSModeDisabled),
		ice.WithProviderConfig(config))
	agent, err := ice.NewAgentWithOptions(options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	// The Agent must own a frozen snapshot rather than reading caller mutations.
	slices.Reverse(config.LocalAddressOrder)
	published := make(chan ice.Candidate, len(config.MappedUDPEndpoints)+len(config.MappedTCPEndpoints)+1)
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
	var candidates []ice.Candidate
	for {
		candidate := await(t, published)
		if candidate == nil {
			return agent, candidates
		}
		candidates = append(candidates, candidate)
	}
}

func TestPriorityAdmissionPreservesTCPDirectionAndInterfaceOpportunities(t *testing.T) {
	order := []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2"),
	}
	// Duplicates and an unlisted base arrive before the other listed bases.
	bases := []netip.Addr{order[0], order[0], netip.MustParseAddr("192.0.2.99"), order[3], order[2], order[1]}
	ranks := []uint32{0, 4, 5, 3, 2, 1}
	var mappings []ice.MappedEndpoint
	for index, base := range bases {
		external := netip.MustParseAddr("203.0.113.1")
		if base.Is6() {
			external = netip.MustParseAddr("2001:db8:ffff::1")
		}
		mappings = append(mappings, ice.MappedEndpoint{
			Local:    netip.AddrPortFrom(base, 4000),
			External: netip.AddrPortFrom(external, uint16(5000+index)),
		})
	}
	const tcpOffset = 7 // A nondefault offset catches scoring before Agent association.
	agent, published := gatherPriorityCandidates(t, ice.ProviderConfig{
		LocalAddressOrder: slices.Clone(order), SrflxMux: priorityFixtureMux{}, TCPMappedMux: priorityFixtureMux{},
		MappedUDPEndpoints: mappings, MappedTCPEndpoints: mappings,
	}, ice.WithTCPPriorityOffset(tcpOffset))
	if len(published) != 2*len(mappings) {
		t.Fatalf("published %d candidates", len(published))
	}
	scores := make(map[uint32]bool)
	for _, candidate := range published {
		index := candidate.Port() - 5000
		// UDP's type preference remains above TCP; TCP passive srflx keeps
		// the existing direction preference (2), even on the highest-ranked IP.
		localPreference := uint32(65535) - ranks[index]
		typePreference := uint32(ice.CandidateTypeServerReflexive.Preference())
		if candidate.NetworkType().IsTCP() {
			localPreference = (2 << 13) + 8191 - ranks[index]
			typePreference -= tcpOffset
			if candidate.TCPType() != ice.TCPTypePassive {
				t.Fatal("TCP direction lost")
			}
		}
		want := typePreference<<24 | localPreference<<8 | 255
		if candidate.Priority() != want || scores[want] {
			t.Fatalf("%s priority=%d, want unique %d", candidate, candidate.Priority(), want)
		}
		scores[want] = true
	}
	assertPriorityPublication(t, agent, published)
}

func assertPriorityPublication(t *testing.T, agent *ice.Agent, published []ice.Candidate) {
	t.Helper()
	local, err := agent.GetLocalCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != len(published) {
		t.Fatalf("local=%d, published=%d", len(local), len(published))
	}
	for _, candidate := range local {
		if !slices.ContainsFunc(published, func(p ice.Candidate) bool { return p.Marshal() == candidate.Marshal() }) {
			t.Fatalf("internal priority differs from published candidate: %s", candidate)
		}
	}
	for _, stats := range agent.GetLocalCandidatesStats() {
		if !slices.ContainsFunc(local, func(c ice.Candidate) bool { return c.ID() == stats.ID && c.Priority() == stats.Priority }) {
			t.Fatalf("stats priority differs from admitted candidate: %+v", stats)
		}
	}
}

func TestPriorityExhaustionDoesNotBorrowAnotherTCPDirection(t *testing.T) {
	// Reserve the full other-pref range using only three candidate admissions.
	// This exercises the boundary without a slow thousands-of-sockets test.
	order := make([]netip.Addr, 8192)
	for index := range order {
		order[index] = netip.AddrFrom4([4]byte{10, 0, byte(index >> 8), byte(index)})
	}
	var mappings []ice.MappedEndpoint
	for index, address := range []netip.Addr{order[0], order[0], order[len(order)-1]} {
		mappings = append(mappings, ice.MappedEndpoint{
			Local: netip.AddrPortFrom(address, 4000), External: netip.AddrPortFrom(address, uint16(5000+index)),
		})
	}
	agent, published := gatherPriorityCandidates(t, ice.ProviderConfig{
		LocalAddressOrder: order, TCPMappedMux: priorityFixtureMux{}, MappedTCPEndpoints: mappings,
	})
	if len(published) != 2 || published[0].Port() != 5000 || published[1].Port() != 5002 {
		t.Fatalf("exhaustion should preserve the reserved final base: %v", published)
	}
	for _, candidate := range published {
		if candidate.Priority()>>21&7 != 2 {
			t.Fatalf("direction corrupted: %s", candidate)
		}
	}
	assertPriorityPublication(t, agent, published)
}
