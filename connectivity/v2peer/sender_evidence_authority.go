package v2peer

import "github.com/windshare/windshare/connectivity/v2signal"

type evidenceClaim struct{ acquired, capacity bool }

const minimumPeerPaths = 1

// The bounded watermark is per stable path, never per attempt. A new path that
// exceeds membership cannot poison already admitted paths or the file session.
type senderEvidenceAuthority struct {
	maximumPaths int
	claims       map[v2signal.Binding]peerOperation
	latest       map[v2signal.PeerPathID]v2signal.Binding
}

func newSenderEvidenceAuthority(maximumPaths int) senderEvidenceAuthority {
	return senderEvidenceAuthority{maximumPaths: maximumPaths, claims: make(map[v2signal.Binding]peerOperation), latest: make(map[v2signal.PeerPathID]v2signal.Binding)}
}
func (a *senderEvidenceAuthority) claim(operation peerOperation, binding v2signal.Binding) evidenceClaim {
	if a == nil || binding.Validate() != nil {
		return evidenceClaim{}
	}
	previous, exists := a.latest[binding.PeerPathID]
	if exists {
		if binding.AttemptSequence <= previous.AttemptSequence {
			return evidenceClaim{}
		}
		delete(a.claims, previous)
	} else if len(a.latest) >= a.maximumPaths {
		return evidenceClaim{capacity: true}
	}
	a.latest[binding.PeerPathID] = binding
	a.claims[binding] = operation
	return evidenceClaim{acquired: true}
}
func (a *senderEvidenceAuthority) claimed(binding v2signal.Binding) bool {
	if a == nil {
		return false
	}
	latest, exists := a.latest[binding.PeerPathID]
	return exists && binding.AttemptSequence <= latest.AttemptSequence
}
func (a *senderEvidenceAuthority) reset() {
	if a != nil {
		clear(a.claims)
		clear(a.latest)
	}
}
func (a *senderEvidenceAuthority) retainedIdentityCount() int {
	if a == nil {
		return 0
	}
	return len(a.latest)
}
