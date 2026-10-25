package relayv2

import (
	"math"
	"testing"
	"time"
)

func TestDiagnosticHeartbeatPreservesSourceStageAndDeadline(t *testing.T) {
	for _, stage := range []LifecycleStage{LifecycleHeartbeatAcknowledged, LifecycleStage("future_heartbeat_stage")} {
		record := LifecycleTrace{Stage: stage, LinkID: math.MaxUint64, HeartbeatRound: 7, Wait: time.Second, Timeout: 45 * time.Second}.DiagnosticRecord()
		p := record.Payload()
		if p.Get("stage").Text() != string(stage) || p.Get("link_id").Uint() != math.MaxUint64 || p.Get("heartbeat_round").Uint() != 7 || p.Get("wait_ms").Uint() != 1000 || p.Get("timeout_ms").Uint() != 45000 {
			t.Fatal("heartbeat stage or timing lost")
		}
	}
}
