// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"net/netip"
	"testing"

	"github.com/pion/logging"
)

func TestProviderPriorityPreservesPeerReflexiveCheckPriority(t *testing.T) {
	const tcpOffset = 19
	const typePreferenceShift = 24
	const localAndComponentMask = (1 << typePreferenceShift) - 1
	for _, test := range []struct {
		name, network string
		tcpType       TCPType
	}{
		{"udp", "udp", TCPTypeUnspecified},
		{"tcp-active", "tcp", TCPTypeActive},
		{"tcp-passive", "tcp", TCPTypePassive},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := &Agent{
				tcpPriorityOffset: tcpOffset,
				providerConfig:    ProviderConfig{LocalAddressOrder: []netip.Addr{netip.MustParseAddr("192.0.2.1")}},
				localCandidates:   make(map[NetworkType][]Candidate),
				log:               logging.NewDefaultLoggerFactory().NewLogger("test"),
			}
			priorities := make(map[uint32]bool)
			for _, port := range []int{4001, 4002} {
				candidate, err := NewCandidateHost(&CandidateHostConfig{
					Network: test.network, Address: "192.0.2.1", Port: port,
					Component: ComponentRTP, TCPType: test.tcpType,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := agent.setLocalCandidatePriority(candidate); err != nil {
					t.Fatal(err)
				}
				admitted := candidate.Priority()
				candidate.currAgent = agent
				if candidate.Priority() != admitted {
					t.Fatal("attaching the agent changed the published priority")
				}
				checkPriority := candidate.prflxPriority()
				wantType := CandidateTypePeerReflexive.Preference()
				if candidate.NetworkType().IsTCP() {
					wantType -= tcpOffset
				}
				if checkPriority>>typePreferenceShift != uint32(wantType) {
					t.Fatalf("STUN check type preference = %d, want %d", checkPriority>>typePreferenceShift, wantType)
				}
				if checkPriority&localAndComponentMask != admitted&localAndComponentMask {
					t.Fatal("STUN check lost the provider's interface or TCP direction preference")
				}
				if priorities[checkPriority] {
					t.Fatal("repeated base addresses reused a peer-reflexive priority")
				}
				priorities[checkPriority] = true
				agent.localCandidates[candidate.NetworkType()] = append(agent.localCandidates[candidate.NetworkType()], candidate)
			}
		})
	}
}
