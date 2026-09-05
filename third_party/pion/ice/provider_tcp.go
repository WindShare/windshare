// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"context"
	"slices"
)

func (a *Agent) gatherProviderTCPEndpoints(ctx context.Context) {
	for _, endpoint := range a.providerConfig.MappedTCPEndpoints {
		if ctx.Err() != nil {
			return
		}
		network := NetworkTypeTCP4
		if endpoint.Local.Addr().Is6() {
			network = NetworkTypeTCP6
		}
		if !slices.Contains(a.networkTypes, network) {
			continue
		}
		conn, err := a.providerConfig.TCPMappedMux.GetConnForEndpoint(a.localUfrag, endpoint.Local)
		if err != nil {
			a.log.Warnf("mapped TCP endpoint socket: %v", err)
			continue
		}
		candidate, err := NewCandidateServerReflexive(&CandidateServerReflexiveConfig{
			Network: network.String(), Address: endpoint.External.Addr().String(), Port: int(endpoint.External.Port()),
			Component: ComponentRTP, RelAddr: endpoint.Local.Addr().String(), RelPort: int(endpoint.Local.Port()), TCPType: TCPTypePassive,
		})
		if err != nil {
			_ = conn.Close()
			a.log.Warnf("mapped TCP endpoint candidate: %v", err)
			continue
		}
		if err = a.addCandidate(ctx, candidate, conn); err != nil {
			_ = conn.Close()
			a.log.Warnf("mapped TCP endpoint admission: %v", err)
		}
	}
}
