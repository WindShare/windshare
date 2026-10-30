// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

func TestProviderMappingsRejectRetiredGatherGeneration(t *testing.T) {
	const retiredGeneration = 0
	const currentGeneration = 1
	const gatheringUfrag = "retired-gathering"
	endpoint := MappedEndpoint{
		Local:    netip.MustParseAddrPort("192.0.2.1:4001"),
		External: netip.MustParseAddrPort("198.51.100.1:5001"),
	}
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			conn := &stunTestPacketConn{closed: make(chan struct{})}
			mux := &generationTestMux{conn: conn}
			config := ProviderConfig{}
			if network == "udp" {
				config.SrflxMux = mux
				config.MappedUDPEndpoints = []MappedEndpoint{endpoint}
			} else {
				config.TCPMappedMux = mux
				config.MappedTCPEndpoints = []MappedEndpoint{endpoint}
			}
			agent, err := NewAgentWithOptions(
				WithMulticastDNSMode(MulticastDNSModeDisabled),
				WithNetworkTypes([]NetworkType{NetworkTypeUDP4, NetworkTypeTCP4}),
				WithProviderConfig(config),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = agent.Close() }()
			if err := agent.loop.Run(context.Background(), func(context.Context) {
				agent.gatherGeneration = currentGeneration
			}); err != nil {
				t.Fatal(err)
			}
			agent.gatherProviderEndpoints(context.Background(), retiredGeneration, gatheringUfrag)
			if mux.ufrag != gatheringUfrag {
				t.Fatal("retired gathering borrowed the current agent's socket identity")
			}
			candidates, err := agent.GetLocalCandidates()
			if err != nil || len(candidates) != 0 {
				t.Fatalf("retired mapping entered the current generation: %v, %v", candidates, err)
			}
			select {
			case <-conn.closed:
			default:
				t.Fatal("rejected mapping retained its socket reference")
			}
		})
	}
}

type generationTestMux struct {
	UniversalUDPMux
	conn  net.PacketConn
	ufrag string
}

func (m *generationTestMux) GetConnForURL(ufrag, _ string, _ net.Addr) (net.PacketConn, error) {
	m.ufrag = ufrag
	return m.conn, nil
}

func (m *generationTestMux) GetConnForEndpoint(ufrag string, _ netip.AddrPort) (net.PacketConn, error) {
	m.ufrag = ufrag
	return m.conn, nil
}

func (*generationTestMux) RemoveConnByUfrag(string) {}
