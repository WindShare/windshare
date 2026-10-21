package runtrace

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestContractRejectionsAndCapacityLossKeepIndependentCounts(t *testing.T) {
	file := &memoryTraceFile{}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
	loss := clievent.TraceLoss{ContractRejections: 2, LifecycleDrops: 3, ProgressDrops: 5}
	if !recorder.ReportUpstreamLoss(loss) {
		t.Fatal("loss accounting rejected")
	}
	health := awaitHealth(t, recorder.Health())
	if health.Cause() != clievent.TraceIncompleteContractRejection || health.Loss() != loss {
		t.Fatalf("health lost rejection cause or independent counts: %+v", health)
	}
	if !recorder.ReportUpstreamLoss(clievent.TraceLoss{LifecycleDrops: 4}) {
		t.Fatal("later queue loss rejected")
	}
	if !recorder.Record(clievent.NewReady()) {
		t.Fatal("incomplete diagnostics stopped subsequent recording")
	}
	status := recorder.Close()
	if status.Complete || status.ContractRejected != 2 || status.LifecycleDropped != 7 || status.ProgressDropped != 5 ||
		status.WriterFailed || status.FlushFailed {
		t.Fatalf("losses were combined or counted twice: %+v", status)
	}
	var summary struct {
		Event   string
		Payload struct {
			Rejected string `json:"contract_rejected"`
			Dropped  string `json:"lifecycle_dropped"`
			Progress string `json:"progress_dropped"`
		}
	}
	records := bytes.Split(bytes.TrimSpace(file.Bytes()), []byte{'\n'})
	encoded := records[len(records)-1]
	if err := json.Unmarshal(encoded, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Event != "trace_summary" || summary.Payload.Rejected != "2" || summary.Payload.Dropped != "7" || summary.Payload.Progress != "5" {
		t.Fatalf("summary lost distinct counts: %s", encoded)
	}
}
