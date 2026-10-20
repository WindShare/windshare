package provider

import (
	"testing"

	"github.com/pion/turn/v5"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session/turncontrol"
)

func TestTURNRecoveryOnlyFollowsSelectedBinding(t *testing.T) {
	var reasons []turncontrol.RecoveryReason
	c := &Connection{request: AttemptConfig{OnTURNRecovery: func(reason turncontrol.RecoveryReason) { reasons = append(reasons, reason) }}}
	fault := turn.BindingEvent{Status: turn.BindingMismatch, Code: 400, RelayedAddress: "127.0.0.1:10", PeerAddress: "127.0.0.1:20"}
	c.observeTURNBinding(fault)
	if len(reasons) != 0 {
		t.Fatal("unused probe started path recovery")
	}
	c.selectTURNPair(&pion.ICECandidatePair{Local: &pion.ICECandidate{Address: "127.0.0.1", Port: 10}, Remote: &pion.ICECandidate{Address: "127.0.0.1", Port: 21}})
	if len(reasons) != 0 {
		t.Fatal("unselected peer binding restarted selected path")
	}
	c.selectTURNPair(&pion.ICECandidatePair{Local: &pion.ICECandidate{Address: "127.0.0.1", Port: 10}, Remote: &pion.ICECandidate{Address: "127.0.0.1", Port: 20}})
	c.observeTURNBinding(fault)
	if len(reasons) != 1 || reasons[0] != turncontrol.AllocationStateMismatch {
		t.Fatal("selected fault was lost or repeated")
	}
}

func TestAllocationLossAffectsSelectedAllocationRegardlessOfProbe(t *testing.T) {
	calls := 0
	c := &Connection{request: AttemptConfig{OnTURNRecovery: func(turncontrol.RecoveryReason) { calls++ }}}
	c.selectTURNPair(&pion.ICECandidatePair{Local: &pion.ICECandidate{Address: "127.0.0.1", Port: 10}, Remote: &pion.ICECandidate{Address: "127.0.0.1", Port: 20}})
	c.observeTURNBinding(turn.BindingEvent{Status: turn.BindingMismatch, Code: 437, RelayedAddress: "127.0.0.1:11", PeerAddress: "127.0.0.1:21"})
	if calls != 0 {
		t.Fatal("another allocation affected selected path")
	}
	c.observeTURNBinding(turn.BindingEvent{Status: turn.BindingMismatch, Code: 437, RelayedAddress: "127.0.0.1:10", PeerAddress: "127.0.0.1:21"})
	if calls != 1 {
		t.Fatal("allocation-level loss was incorrectly isolated to a probe")
	}
}

func TestExpiredBindingCanFinishItsBoundedRetryBeforePathRecovery(t *testing.T) {
	calls := 0
	c := &Connection{request: AttemptConfig{OnTURNRecovery: func(turncontrol.RecoveryReason) { calls++ }}}
	c.selectTURNPair(&pion.ICECandidatePair{Local: &pion.ICECandidate{Address: "127.0.0.1", Port: 10}, Remote: &pion.ICECandidate{Address: "127.0.0.1", Port: 20}})
	for _, status := range []turn.BindingStatus{turn.BindingExpired, turn.BindingRetrying, turn.BindingActive} {
		c.observeTURNBinding(turn.BindingEvent{Status: status, RelayedAddress: "127.0.0.1:10", PeerAddress: "127.0.0.1:20"})
	}
	if calls != 0 {
		t.Fatal("a successful refresh unnecessarily replaced the path")
	}
	c.observeTURNBinding(turn.BindingEvent{Status: turn.BindingExhausted, RelayedAddress: "127.0.0.1:10", PeerAddress: "127.0.0.1:20"})
	if calls != 1 {
		t.Fatal("exhausted refresh did not request recovery")
	}
}
