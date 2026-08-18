package v2peer

import "github.com/windshare/windshare/connectivity/v2signal"

type evidenceClaim struct {
	acquired        bool
	sessionTerminal bool
}

const (
	senderEvidenceTerminalIdentityReserve = 1
	minimumSessionEvidenceIdentities      = SenderMaxActivePeerAttemptsPerSession +
		senderEvidenceTerminalIdentityReserve
)

// senderEvidenceAuthority is protected by senderHandler.mu. Exact membership
// must outlive replay tombstones, but its memory cannot. One identity is reserved
// for the capacity terminal so ordinary claims and their terminal boundary share
// the same session-wide ceiling.
type senderEvidenceAuthority struct {
	maximumIdentities int
	claims            map[v2signal.Binding]peerOperation
	terminal          bool
	terminalBinding   v2signal.Binding
}

func newSenderEvidenceAuthority(maximumIdentities int) senderEvidenceAuthority {
	return senderEvidenceAuthority{
		maximumIdentities: maximumIdentities,
		claims: make(
			map[v2signal.Binding]peerOperation,
			maximumIdentities-senderEvidenceTerminalIdentityReserve,
		),
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
	if len(authority.claims)+senderEvidenceTerminalIdentityReserve < authority.maximumIdentities {
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

func (authority *senderEvidenceAuthority) retainedIdentityCount() int {
	if authority == nil {
		return 0
	}
	count := len(authority.claims)
	if authority.terminal {
		count++
	}
	return count
}
