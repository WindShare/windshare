package cli

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

func runtimeRejectionSample() clievent.ObservationRejection {
	return clievent.CaptureObservationRejection(clievent.ObservationRejection{
		Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation",
		Stage: "unknown", Field: "stage", Rule: "known_enum",
	}, clievent.RejectedEnum("stage", 255))
}

func TestRejectionEvidenceFailureRetainsIndependentNumericLoss(t *testing.T) {
	for _, test := range []struct {
		name         string
		breakRuntime func(*commandRuntime)
	}{
		{"exhausted_command_sequence", func(runtime *commandRuntime) { runtime.entrySequence = ^uint64(0) }},
		{"invalid_record_construction", func(runtime *commandRuntime) { runtime.command = 255 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := rejectionRuntime()
			test.breakRuntime(runtime)
			for range 5 {
				if !runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, runtimeRejectionSample()) {
					t.Fatal("original rejection was not accounted")
				}
			}
			loss := runtime.collectPendingLossLocked(true)
			runtime.reportRejectionEvidenceLoss()
			recorder := runtime.trace.(*fakeUserTrace)
			if loss.lifecycle != 5 || runtime.rejectionEvidenceDropped.Load() != 5 ||
				recorder.rejectionEvidence != 5 || runtime.pendingRejectionEvidenceLoss.Load() != 0 {
				t.Fatalf("loss=%+v evidence=%d recorder=%d", loss, runtime.rejectionEvidenceDropped.Load(), recorder.rejectionEvidence)
			}
			if runtime.takeNext() != nil || len(runtime.projectionRejections) != 1 {
				t.Fatal("anomaly failure recursively created an observation")
			}
			if later := runtime.collectPendingLossLocked(true); later.lifecycle != 0 {
				t.Fatal("original observation loss was counted twice")
			}
		})
	}
}

func TestMalformedRejectionSampleRetainsLossWithoutRecursiveDiagnostics(t *testing.T) {
	runtime, _ := rejectionRuntime()
	if runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, clievent.ObservationRejection{}) {
		t.Fatal("malformed sample accepted")
	}
	loss := runtime.collectPendingLossLocked(true)
	runtime.reportRejectionEvidenceLoss()
	if loss.lifecycle != 1 || runtime.rejectionEvidenceDropped.Load() != 1 ||
		runtime.trace.(*fakeUserTrace).rejectionEvidence != 1 || len(runtime.projectionRejections) != 0 || runtime.takeNext() != nil {
		t.Fatalf("malformed rejection loss=%+v evidence=%d", loss, runtime.rejectionEvidenceDropped.Load())
	}
}

func TestFailedEvidenceHealthUpdatePreservesPendingCounter(t *testing.T) {
	runtime, _ := rejectionRuntime()
	recorder := runtime.trace.(*fakeUserTrace)
	recorder.rejectEvidenceLoss = true
	runtime.loseRejectionEvidenceLocked(7)
	runtime.reportRejectionEvidenceLoss()
	if runtime.pendingRejectionEvidenceLoss.Load() != 7 || !runtime.rejectionEvidenceLossReportFailed.Load() ||
		runtime.rejectionEvidenceDropped.Load() != 7 || runtime.takeNext() != nil {
		t.Fatal("failed numeric health update silently erased evidence loss")
	}
	recorder.rejectEvidenceLoss = false
	runtime.reportRejectionEvidenceLoss()
	runtime.reportRejectionEvidenceLoss()
	if recorder.rejectionEvidence != 7 || runtime.pendingRejectionEvidenceLoss.Load() != 0 {
		t.Fatal("numeric evidence retry lost or duplicated the count")
	}
}

func TestRejectionSignaturesIncludeEventAndSourceButExcludeValues(t *testing.T) {
	runtime, _ := rejectionRuntime()
	sample := runtimeRejectionSample()
	runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, sample)
	sample.Source = "other.ProjectProtocolObservation"
	runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, sample)
	sample.Event = "protocol_send_attempt_settled"
	runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, sample)
	for i := range 100 {
		sample = clievent.CaptureObservationRejection(sample, clievent.RejectedEnum("stage", uint64(i)))
		runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, sample)
	}
	if len(runtime.projectionRejections) != 3 || runtime.projectionRejections[2].count != 101 {
		t.Fatal("source signatures merged or actual values grew signature cardinality")
	}
	runtime.projectionRejections[2].count = ^uint64(0)
	runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossUnknownEnum, sample)
	if runtime.projectionRejections[2].count != ^uint64(0) {
		t.Fatal("repeat delta wrapped")
	}
}

func TestRejectionEvidenceHealthHasDistinctCause(t *testing.T) {
	event := traceIncompleteFromStatus(clievent.CommandShare, runtrace.Status{RejectionEvidenceDropped: 4})
	if event.Cause() != clievent.TraceIncompleteRejectionEvidence || event.LifecycleDrops() != 0 {
		t.Fatal("evidence loss was misrepresented as another original lifecycle loss")
	}
}
