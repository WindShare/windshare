package cli

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

func rejectionRuntime() (*commandRuntime, *fakeCommandClock) {
	clock := &fakeCommandClock{now: time.Unix(1, 0)}
	return &commandRuntime{
		command: clievent.CommandShare, clock: clock,
		trace: newFakeUserTrace(runtrace.Status{Complete: true}), ready: make(chan struct{}, 1),
	}, clock
}

func TestProjectionRejectionReportsFirstSampleAndFlushesExactCounts(t *testing.T) {
	runtime, clock := rejectionRuntime()
	input := clievent.CaptureObservationRejection(clievent.ObservationRejection{
		Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation",
		Stage: "sender_request_received", Field: "protocol_operation_id", Rule: "nonzero_16_bytes",
		Session: "01000000000000000000000000000000", Operation: "00000000000000000000000000000000",
	}, clievent.RejectedEnum("request_kind", 3))
	report := func() {
		t.Helper()
		if !runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossInvalidIdentity, input) {
			t.Fatal("rejection not accounted")
		}
	}
	report()
	if loss := runtime.collectPendingLossLocked(false); loss.lifecycle != 1 {
		t.Fatalf("first loss = %+v", loss)
	}
	first, ok := runtime.takeNext().(clievent.ObserverLossObserved)
	if !ok || first.Count() != 1 {
		t.Fatalf("first report = %#v", first)
	}
	sample, ok := first.Rejection()
	if !ok || sample.Stage != "sender_request_received" || sample.Field != "protocol_operation_id" ||
		sample.Rule != "nonzero_16_bytes" || sample.Session != input.Session || sample.Operation != input.Operation {
		t.Fatalf("rejection sample = %+v", sample)
	}

	// Changing operation/session identities must not reset the signature limit.
	for i := range 100 {
		input.Session = fmt.Sprintf("session-%d", i)
		input.ResponseSequence = fmt.Sprintf("%d", i)
		input = clievent.CaptureObservationRejection(input, clievent.RejectedEnum("request_kind", uint64(i)))
		report()
		if loss := runtime.collectPendingLossLocked(false); loss.lifecycle != 1 {
			t.Fatalf("loss = %+v", loss)
		}
	}
	if runtime.takeNext() != nil {
		t.Fatal("repeated failures bypassed the report interval")
	}
	clock.now = clock.now.Add(projectionRejectionReportInterval)
	if loss := runtime.collectPendingLossLocked(false); loss.lifecycle != 0 {
		t.Fatal("counted the same failures twice")
	}
	repeated := runtime.takeNext().(clievent.ObserverLossObserved)
	repeatedSample, _ := repeated.Rejection()
	if repeated.Count() != 100 || repeatedSample != sample {
		t.Fatalf("repeat report = %#v", repeated)
	}
	report()
	report()
	terminal := newRuntimeTestCommandFailure(t, clievent.CommandShare, clievent.FailureCanceled)
	if !runtime.Finalize(terminal) {
		t.Fatal("finalize failed")
	}
	final := runtime.takeNext().(clievent.ObserverLossObserved)
	if final.Count() != 2 || runtime.takeNext() != terminal || runtime.takeNext() != nil {
		t.Fatalf("final report ordering/count = %#v", final)
	}
	if loss := runtime.collectPendingLossLocked(true); loss.lifecycle != 0 {
		t.Fatal("final flush duplicated losses")
	}
}

func TestProjectionRejectionSignatureCapacity(t *testing.T) {
	runtime, _ := rejectionRuntime()
	const count = 100
	for i := range count {
		sample := clievent.ObservationRejection{Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: fmt.Sprintf("stage_%d", i), Field: "field", Rule: "rule"}
		if !runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossEventContract, sample) {
			t.Fatal("rejection not accounted")
		}
	}
	if len(runtime.projectionRejections) != maxProjectionRejectionSignatures {
		t.Fatal("signature memory is not bounded")
	}
	if loss := runtime.collectPendingLossLocked(true); loss.lifecycle != count {
		t.Fatalf("loss = %+v", loss)
	}
	var total, omitted uint64
	reports := 0
	for event := runtime.takeNext(); event != nil; event = runtime.takeNext() {
		total += event.(clievent.ObserverLossObserved).Count()
		omitted += event.(clievent.ObserverLossObserved).OmittedSamples()
		reports++
	}
	if total != count || reports != maxProjectionRejectionSignatures || omitted != count-(maxProjectionRejectionSignatures-1) {
		t.Fatalf("total=%d reports=%d", total, reports)
	}
}

func TestProjectionRejectionDisabledTraceRetainsNoSamples(t *testing.T) {
	runtime, _ := rejectionRuntime()
	runtime.trace = nil
	sample := clievent.ObservationRejection{Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "stage", Field: "field", Rule: "rule"}
	for range 100 {
		runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossEventContract, sample)
	}
	if len(runtime.projectionRejections) != 0 {
		t.Fatal("disabled trace retained rejection samples")
	}
	if loss := runtime.collectPendingLossLocked(true); loss.lifecycle != 100 || runtime.takeNext() != nil {
		t.Fatalf("loss=%+v", loss)
	}
	runtime.closed = true
	if runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossEventContract, sample) {
		t.Fatal("closed runtime accepted a rejection")
	}
}

func BenchmarkObservationRejectionRepeated(b *testing.B) {
	runtime, _ := rejectionRuntime()
	sample := clievent.ObservationRejection{Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "sender_request_received", Field: "protocol_operation_id", Rule: "nonzero_16_bytes"}
	runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossInvalidIdentity, sample)
	b.ReportAllocs()
	for b.Loop() {
		runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossInvalidIdentity, sample)
	}
}

func TestObservationRejectionConcurrentAccounting(t *testing.T) {
	runtime, _ := rejectionRuntime()
	sample := clievent.ObservationRejection{Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "stage", Field: "field", Rule: "rule"}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 32 {
				runtime.ReportObservationRejection(clievent.ObserverLossProtocolOperation, clievent.ObserverLossEventContract, sample)
			}
		})
	}
	workers.Wait()
	if loss := runtime.collectPendingLossLocked(true); loss.lifecycle != 256 {
		t.Fatalf("loss=%+v", loss)
	}
	event := runtime.takeNext().(clievent.ObserverLossObserved)
	if event.Count() != 256 || runtime.takeNext() != nil {
		t.Fatal("concurrent counts were lost or duplicated")
	}
}
