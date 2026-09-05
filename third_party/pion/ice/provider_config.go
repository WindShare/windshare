// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"
)

// MappedEndpoint binds a verified externally allocated endpoint to its real
// local socket. It is immutable for one Agent; late leases need a fresh Agent.
type MappedEndpoint struct {
	Local    netip.AddrPort
	External netip.AddrPort
}

// ProviderConfig supplies capabilities whose lifetime belongs to the embedding
// application. No retry, lease renewal, or network monitoring is owned here.
type ProviderConfig struct {
	SrflxMux               UniversalUDPMux
	InitialCheckingTimeout time.Duration
	MappedUDPEndpoints     []MappedEndpoint
	MappedTCPEndpoints     []MappedEndpoint
	TCPMappedMux           interface {
		GetConnForEndpoint(string, netip.AddrPort) (net.PacketConn, error)
	}
	LocalPreference func(Candidate) (uint16, bool)
}

// WithProviderConfig preserves legacy behavior when the config is empty.
func WithProviderConfig(config ProviderConfig) AgentOption {
	config.MappedUDPEndpoints = slices.Clone(config.MappedUDPEndpoints)
	config.MappedTCPEndpoints = slices.Clone(config.MappedTCPEndpoints)
	return func(a *Agent) error {
		if config.InitialCheckingTimeout < 0 {
			return fmt.Errorf("negative initial ICE checking timeout")
		}
		for _, endpoint := range config.MappedUDPEndpoints {
			if !endpoint.Local.IsValid() || !endpoint.External.IsValid() ||
				endpoint.Local.Port() == 0 || endpoint.External.Port() == 0 ||
				endpoint.Local.Addr().Is4() != endpoint.External.Addr().Is4() {
				return fmt.Errorf("invalid mapped UDP endpoint")
			}
			if config.SrflxMux == nil {
				return fmt.Errorf("mapped UDP endpoint requires socket mux")
			}
		}
		for _, endpoint := range config.MappedTCPEndpoints {
			if !endpoint.Local.IsValid() || !endpoint.External.IsValid() || endpoint.Local.Port() == 0 || endpoint.External.Port() == 0 || endpoint.Local.Addr().Is4() != endpoint.External.Addr().Is4() || config.TCPMappedMux == nil {
				return fmt.Errorf("invalid mapped TCP endpoint")
			}
		}
		a.providerConfig = config
		if config.SrflxMux != nil {
			a.udpMuxSrflx = config.SrflxMux
		}
		return nil
	}
}

func (a *Agent) initialCheckingTimeout() time.Duration {
	if a.providerConfig.InitialCheckingTimeout > 0 {
		return a.providerConfig.InitialCheckingTimeout
	}
	return a.disconnectedTimeout + a.failedTimeout
}

func (a *Agent) gatherProviderEndpoints(ctx context.Context) {
	a.gatherProviderTCPEndpoints(ctx)
	for _, endpoint := range a.providerConfig.MappedUDPEndpoints {
		if ctx.Err() != nil {
			return
		}
		local := net.UDPAddrFromAddrPort(endpoint.Local)
		network := NetworkTypeUDP6
		if endpoint.Local.Addr().Is4() {
			network = NetworkTypeUDP4
		}
		if !slices.Contains(a.networkTypes, network) {
			continue
		}
		// A separate mux reference allows candidate disposal without closing either
		// the physical endpoint or another local candidate's reference.
		conn, err := a.providerConfig.SrflxMux.GetConnForURL(a.localUfrag, "mapping:"+endpoint.External.String(), local)
		if err != nil {
			a.log.Warnf("mapped endpoint socket: %v", err)
			continue
		}
		candidate, err := NewCandidateServerReflexive(&CandidateServerReflexiveConfig{
			Network: network.String(), Address: endpoint.External.Addr().String(), Port: int(endpoint.External.Port()),
			Component: ComponentRTP, RelAddr: endpoint.Local.Addr().String(), RelPort: int(endpoint.Local.Port()),
		})
		if err != nil {
			_ = conn.Close()
			a.log.Warnf("mapped endpoint candidate: %v", err)
			continue
		}
		if err = a.addCandidate(ctx, candidate, conn); err != nil {
			_ = conn.Close()
			a.log.Warnf("mapped endpoint admission: %v", err)
		}
	}
}
