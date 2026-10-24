package runtrace

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

func rejectionTraceEvent(t *testing.T, count uint64) appobs.ObserverLossObserved {
	t.Helper()
	sample := appobs.CaptureObservationRejection(appobs.ObservationRejection{Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "unknown", Field: "role", Rule: "known_enum", Session: strings.Repeat("0", 32)}, appobs.RejectedEnum("role", 255))
	event, err := appobs.NewObserverLossObserved(appobs.ObserverLossSpec{Workflow: appobs.WorkflowShare, Category: appobs.ObserverLossProtocolOperation, Reason: appobs.ObserverLossUnknownEnum, Count: count, Rejection: sample})
	if err != nil {
		t.Fatal(err)
	}
	return event
}
func TestRecorderPreservesRejectionEvidenceAndIndependentLossCount(t *testing.T) {
	file := &memoryTraceFile{}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
	event := rejectionTraceEvent(t, 7)
	if !recorder.Record(event) || !recorder.ReportRejectionEvidenceLoss(11) {
		t.Fatal("loss evidence rejected")
	}
	status := recorder.Close()
	if status.Complete || status.RejectionEvidenceDropped != 11 || status.LifecycleDropped != 0 {
		t.Fatalf("evidence loss conflated: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	data := string(file.Bytes())
	if len(records) != 2 || !strings.Contains(data, "\"rejection_evidence_dropped\":\"11\"") || !strings.Contains(data, "\"value\":\"255\"") || !strings.Contains(data, "\"omitted_samples\":\"0\"") {
		t.Fatalf("missing rejection evidence: %s", data)
	}
	if recorder.Record(event) || recorder.ReportRejectionEvidenceLoss(1) {
		t.Fatal("closed recorder accepted evidence")
	}
	if recorder.Status().RejectionEvidenceDropped != 11 {
		t.Fatal("post-close submission mutated final status")
	}
}
func TestRecorderCountsRejectedEvidenceAdmissionAndDisabledDrain(t *testing.T) {
	file := &memoryTraceFile{firstWriteEntered: make(chan struct{}), releaseFirstWrite: make(chan struct{}), failWrites: 1}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{LifecycleCapacity: 1}, fixedClock(), file, newManualTicker())
	if !recorder.Record(rejectionTraceEvent(t, 2)) {
		t.Fatal("first evidence not admitted")
	}
	<-file.firstWriteEntered
	if !recorder.Record(rejectionTraceEvent(t, 3)) {
		t.Fatal("queued evidence not admitted")
	}
	if recorder.Record(rejectionTraceEvent(t, 5)) {
		t.Fatal("full queue accepted evidence")
	}
	close(file.releaseFirstWrite)
	status := recorder.Close()
	if status.RejectionEvidenceDropped != 10 || !status.WriterFailed || status.Complete {
		t.Fatalf("admission/write/drain evidence loss: %+v", status)
	}
}
