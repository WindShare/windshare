package revisioncapacity

import (
	"errors"
	"math"
	"testing"
)

func TestDiagnosticRetainsCapacityOwnerAndExactCounters(t *testing.T) {
	scope := ScopeSnapshot{scope: CapacityScopeSession, identity: "session", used: CapacityUsage{StableHandles: math.MaxUint64, ActiveLeases: 1}, limits: CapacityLimits{StableHandles: math.MaxUint64, ActiveLeases: 2}, pending: 3, reclaims: 4, quarantined: 5, reclaimable: 6}
	event := TraceEvent{stage: TraceReclaimQuarantined, storeID: "store", shareID: "share", sessionID: "session", decision: "decision-17", revision: "file:revision", claim: "claim-18", candidate: "candidate-19",
		snapshot: CapacitySnapshot{process: scope, share: ScopeSnapshot{scope: CapacityScopeShare}, sessions: []ScopeSnapshot{scope}}, diagnostic: errors.New("native close failed")}
	record := event.DiagnosticRecord()
	p := record.Payload()
	event.snapshot.sessions[0].used.ActiveLeases = 99
	if p.Get("decision_id").Text() != "decision-17" || p.Get("revision_id").Text() != "file:revision" || p.Get("capacity_store_id").Text() != "store" ||
		p.Get("session").Get("stable_handles").Uint() != math.MaxUint64 || p.Get("session").Get("active_leases").Uint() != 1 ||
		p.Get("session").Get("quarantined_stable_handles").Uint() != 5 || p.Get("error").Text() != "native close failed" || record.Correlation().Get("protocol_session_id").Text() != "session" {
		t.Fatal("capacity evidence changed")
	}
}
