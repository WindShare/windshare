// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"fmt"
	"net/netip"
	"slices"
)

const (
	candidateLocalPreferenceShift = 8
	tcpOtherPreferenceBits        = 13
	maxTCPAddressPreference       = (1 << tcpOtherPreferenceBits) - 1
)

func validateLocalAddressOrder(addresses []netip.Addr) error {
	// Reserve at least one score per base even in TCP's narrower other-pref
	// space. Never truncate a rank into a different direction preference.
	if len(addresses) > maxTCPAddressPreference+1 {
		return fmt.Errorf("local address order exceeds TCP preference space")
	}
	seen := make(map[netip.Addr]bool, len(addresses))
	for _, address := range addresses {
		if !address.IsValid() || address.Is4In6() || seen[address] {
			return fmt.Errorf("invalid or duplicate local preference address: %s", address)
		}
		seen[address] = true
	}
	return nil
}

// setLocalCandidatePriority runs on the agent loop before either admission path
// starts the candidate, creates pairs, or publishes it. A published score must
// never change when another interface or another active TCP connection arrives.
func (a *Agent) setLocalCandidatePriority(candidate Candidate) error {
	addresses := a.providerConfig.LocalAddressOrder
	if len(addresses) == 0 || candidate.Type() == CandidateTypeRelay {
		// TURN has its own relay-protocol preference; base-address ordering must
		// not overwrite that policy either.
		candidate.setPriority(candidate.priorityForAgent(a))
		a.setUniqueLiteCandidatePriority(candidate)
		return nil
	}

	maxPreference := uint32(defaultLocalPreference)
	if candidate.NetworkType().IsTCP() {
		maxPreference = maxTCPAddressPreference
	}
	mask := maxPreference << candidateLocalPreferenceShift
	prefix := candidate.priorityForAgent(a) &^ mask
	used := make(map[uint32]bool)
	for _, candidates := range a.localCandidates {
		for _, existing := range candidates {
			if priority := existing.Priority(); priority&^mask == prefix {
				used[(priority&mask)>>candidateLocalPreferenceShift] = true
			}
		}
	}

	base := candidate.addrPort().Addr().Unmap()
	if related := candidate.RelatedAddress(); related != nil && related.Address != "" {
		address, err := netip.ParseAddr(related.Address)
		if err != nil {
			return fmt.Errorf("candidate %s has invalid preference base: %w", candidate.ID(), err)
		}
		base = address.Unmap()
	}
	rank := slices.Index(addresses, base)
	preference := int(maxPreference) - rank
	secondary := rank < 0 || used[uint32(preference)]
	if secondary {
		// Each listed base owns its first slot regardless of arrival order.
		// Extra ports/mappings and unlisted bases share the remaining space;
		// they cannot steal a late interface's first opportunity.
		preference = int(maxPreference) - len(addresses)
		for preference >= 0 && used[uint32(preference)] {
			preference--
		}
	}
	if preference < 0 {
		a.log.Warnf("candidate_priority_exhausted ufrag=%s candidate_id=%s base=%s tcp_type=%s prefix=%d address_rank=%d",
			a.localUfrag, candidate.ID(), base, candidate.TCPType(), prefix, rank)
		return fmt.Errorf("candidate %s exhausted local preference space", candidate.ID())
	}

	priority := prefix | uint32(preference)<<candidateLocalPreferenceShift
	candidate.setPriority(priority)
	a.log.Tracef("candidate_priority ufrag=%s candidate_id=%s base=%s tcp_type=%s priority=%d address_rank=%d secondary=%t",
		a.localUfrag, candidate.ID(), base, candidate.TCPType(), priority, rank, secondary)
	return nil
}
