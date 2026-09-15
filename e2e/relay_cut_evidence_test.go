package e2e

import (
	"encoding/json"
	"testing"
	"time"
)

type relayCutLane struct {
	session string
	id      uint32
	epoch   uint32
}

func relayCutLaneOf(record v4TraceRecord) (relayCutLane, bool) {
	correlation := record.Correlation
	if correlation == nil || correlation.ProtocolSessionID == "" ||
		correlation.LaneID == nil || correlation.LaneEpoch == nil {
		return relayCutLane{}, false
	}
	return relayCutLane{correlation.ProtocolSessionID, *correlation.LaneID, *correlation.LaneEpoch}, true
}

func hasV2DirectRevisionAfter(t *testing.T, records []v4TraceRecord, cutCompletedAt time.Time) bool {
	t.Helper()
	delivering := make(map[relayCutLane]struct{})
	for _, record := range records {
		if record.Event != "lane_settlement" || v4TraceStringField(t, record.Payload, "route") != "direct" ||
			v4TraceDecimalField(t, record.Payload, "delivered_blocks") == 0 ||
			v4TraceDecimalField(t, record.Payload, "delivered_bytes") == 0 ||
			v4TraceBoolField(t, record.Payload, "incomplete") {
			continue
		}
		if lane, ok := relayCutLaneOf(record); ok {
			delivering[lane] = struct{}{}
		}
	}
	for _, record := range records {
		if record.Event != "protocol_operation" ||
			v4TraceStringField(t, record.Payload, "role") != "receiver" ||
			v4TraceStringField(t, record.Payload, "stage") != "receiver_completed" ||
			v4TraceStringField(t, record.Payload, "request_kind") != "open_revisions" ||
			v4TraceStringField(t, record.Payload, "response_kind") != "open_results" ||
			v4TraceStringField(t, record.Payload, "cause") != "none" {
			continue
		}
		lane, ok := relayCutLaneOf(record)
		if _, delivered := delivering[lane]; !ok || !delivered {
			continue
		}
		observedAt, err := time.Parse(time.RFC3339Nano, v4TraceStringField(t, record.Payload, "observed_at"))
		if err != nil {
			t.Fatalf("invalid revision observation time: %v", err)
		}
		// OpenRevision records completion before returning the lease needed to
		// request content. Use that source time, not the buffered log's write
		// time, to exclude transfers already reduced to output cleanup at the cut.
		if observedAt.After(cutCompletedAt) {
			return true
		}
	}
	return false
}

func TestRelayCutEvidenceRequiresNewContentAfterCut(t *testing.T) {
	cut := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		change func([]v4TraceRecord)
		want   bool
	}{
		{name: "mixed recent traffic with direct delivery", want: true},
		{name: "revision before cut despite later log delivery", change: func(records []v4TraceRecord) {
			records[0].Payload["observed_at"] = relayCutString(cut.Add(-time.Second).Format(time.RFC3339Nano))
		}},
		{name: "revision at cut", change: func(records []v4TraceRecord) {
			records[0].Payload["observed_at"] = relayCutString(cut.Format(time.RFC3339Nano))
		}},
		{name: "only lease cleanup after cut", change: func(records []v4TraceRecord) {
			records[0].Payload["request_kind"] = relayCutString("release_lease")
		}},
		{name: "revision failed", change: func(records []v4TraceRecord) {
			records[0].Payload["stage"] = relayCutString("receiver_failed")
		}},
		{name: "relay delivery", change: func(records []v4TraceRecord) {
			records[1].Payload["route"] = relayCutString("relay")
		}},
		{name: "direct lane never delivered", change: func(records []v4TraceRecord) {
			records[1].Payload["delivered_bytes"] = relayCutString("0")
		}},
		{name: "incomplete delivery evidence", change: func(records []v4TraceRecord) {
			records[1].Payload["incomplete"] = json.RawMessage("true")
		}},
		{name: "different session", change: func(records []v4TraceRecord) {
			records[0].Correlation.ProtocolSessionID = "other-session"
		}},
		{name: "different lane incarnation", change: func(records []v4TraceRecord) {
			epoch := uint32(2)
			records[0].Correlation.LaneEpoch = &epoch
		}},
		{name: "missing lane correlation", change: func(records []v4TraceRecord) {
			records[0].Correlation = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := relayCutEvidenceFixture(cut.Add(time.Second))
			if test.change != nil {
				test.change(records)
			}
			if got := hasV2DirectRevisionAfter(t, records, cut); got != test.want {
				t.Fatalf("direct content after cut = %t, want %t", got, test.want)
			}
		})
	}
}

func relayCutEvidenceFixture(observedAt time.Time) []v4TraceRecord {
	id, epoch := uint32(2), uint32(1)
	correlation := v4TraceCorrelation{ProtocolSessionID: "session", LaneID: &id, LaneEpoch: &epoch}
	settlementCorrelation := correlation
	return []v4TraceRecord{
		{Event: "protocol_operation", Correlation: &correlation, Payload: map[string]json.RawMessage{
			"role": relayCutString("receiver"), "stage": relayCutString("receiver_completed"),
			"request_kind": relayCutString("open_revisions"), "response_kind": relayCutString("open_results"),
			"cause": relayCutString("none"), "observed_at": relayCutString(observedAt.Format(time.RFC3339Nano)),
		}},
		{Event: "lane_settlement", Correlation: &settlementCorrelation, Payload: map[string]json.RawMessage{
			"route": relayCutString("direct"), "delivered_blocks": relayCutString("1"),
			"delivered_bytes": relayCutString("4096"), "incomplete": json.RawMessage("false"),
		}},
		{Event: "content_path_selected", Payload: map[string]json.RawMessage{
			"content_path": relayCutString("direct_and_relay"),
		}},
	}
}

func relayCutString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
