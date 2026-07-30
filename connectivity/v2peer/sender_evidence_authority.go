package v2peer

import "github.com/windshare/windshare/connectivity/v2signal"

type evidenceClaim struct {
	acquired        bool
	sessionTerminal bool
}

// senderEvidenceAuthority is protected by senderHandler.mu. Exact membership
// must outlive replay tombstones, but its memory cannot. The first identity past
// the normal budget owns one separate terminal slot; publishing that stream and
// ending the ProtocolSession preserves exactness without retaining later input.
type senderEvidenceAuthority struct {
	maximumClaims   int
	claims          map[v2signal.Binding]peerOperation
	terminal        bool
	terminalBinding v2signal.Binding
}

func newSenderEvidenceAuthority(maximumClaims int) senderEvidenceAuthority {
	return senderEvidenceAuthority{
		maximumClaims: maximumClaims,
		claims:        make(map[v2signal.Binding]peerOperation, maximumClaims),
	}
}

func (authority *senderEvidenceAuthority) claim(
	operation peerOperation,
	binding v2signal.Binding,
) evidenceClaim {
	if authority == nil || binding.Validate() != nil {
		return evidenceClaim{}
	}
	if _, exists := authority.claims[binding]; exists {
		return evidenceClaim{sessionTerminal: authority.terminal}
	}
	if authority.terminal {
		return evidenceClaim{sessionTerminal: true}
	}
	if len(authority.claims) < authority.maximumClaims {
		authority.claims[binding] = operation
		return evidenceClaim{acquired: true}
	}
	authority.terminal = true
	authority.terminalBinding = binding
	return evidenceClaim{acquired: true, sessionTerminal: true}
}

func (authority *senderEvidenceAuthority) claimed(binding v2signal.Binding) bool {
	if authority == nil {
		return false
	}
	if _, exists := authority.claims[binding]; exists {
		return true
	}
	return authority.terminal && authority.terminalBinding == binding
}

func (authority *senderEvidenceAuthority) reset() {
	if authority == nil {
		return
	}
	clear(authority.claims)
	authority.terminal = false
	authority.terminalBinding = v2signal.Binding{}
}

func (authority *senderEvidenceAuthority) claimCount() int {
	if authority == nil {
		return 0
	}
	return len(authority.claims)
}
