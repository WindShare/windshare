package runtrace

import (
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestProgressPayloadRecordsRawCapacityWaitIndependentOfPresentation(t *testing.T) {
	for _, visible := range []bool{false, true} {
		snapshot, err := clievent.NewProgressSnapshot(clievent.ProgressSpec{
			Discovery: clievent.DiscoveryComplete, CountersExact: true,
			CapacityActiveWaiters: 2, CapacityAccumulatedWait: 1250 * time.Millisecond,
			CapacityWaitAttempts: 3, CapacityWaitVisible: visible,
		})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := projectProgress(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if payload.CapacityWait.ActiveWaiters != "2" ||
			payload.CapacityWait.AccumulatedWaitMS != "1250" ||
			payload.CapacityWait.Attempts != "3" {
			t.Fatalf("capacity wait payload = %+v", payload.CapacityWait)
		}
	}
}

func TestTransferCapacityLifecyclePayloadKeepsWaitAndProtocolCorrelation(t *testing.T) {
	receive, _ := clievent.NewReceiveOperationID(identityBytes(0x11))
	session, _ := clievent.NewProtocolSessionID(identityBytes(0x12))
	job, _ := clievent.NewTransferJobID(identityBytes(0x13))
	wait, _ := clievent.NewCapacityWaitID(identityBytes(0x14))
	generation, _ := clievent.NewCapacityGenerationID(identityBytes(0x15))
	operation, _ := clievent.NewProtocolOperationID(identityBytes(0x16))
	progress, err := clievent.NewProgressSnapshot(clievent.ProgressSpec{
		Discovery: clievent.DiscoveryOpen, CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewTransferLifecycleObserved(clievent.TransferLifecycleSpec{
		ReceiveOperation: receive, ProtocolSession: session, TransferJob: job,
		Stage: clievent.TransferCapacityRetryScheduled, Progress: progress,
		FileSelection: clievent.FileSelectionNone, FileSettlement: clievent.FileSettlementNone,
		ItemBlockReason: clievent.ItemBlockNone, TreeSettlement: clievent.TreeSettlementNone,
		CapacityWait: wait, CapacityGeneration: generation, CapacityOperation: operation,
		CapacityAttempt: 2, CapacityHint: 250 * time.Millisecond,
		CapacityJitter: 9 * time.Millisecond, CapacityDelay: 259 * time.Millisecond,
		CapacityAccumulatedWait: 400 * time.Millisecond, CapacityActiveWaiters: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV3{}
	visitor := &encodeVisitorV3{record: record}
	if err := visitor.VisitTransferLifecycleObserved(event); err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(transferLifecyclePayloadV3)
	if !ok || payload.Capacity == nil || payload.Capacity.WaitID != encodeTypedIdentity(wait.Bytes()) ||
		payload.Capacity.GenerationID != encodeTypedIdentity(generation.Bytes()) ||
		payload.Capacity.ProtocolOperationID != encodeTypedIdentity(operation.Bytes()) ||
		payload.Capacity.Attempt != "2" || payload.Capacity.HintMS != "250" ||
		payload.Capacity.JitterMS != "9" || payload.Capacity.DelayMS != "259" ||
		payload.Capacity.AccumulatedWaitMS != "400" || payload.Capacity.ActiveWaiters != 3 ||
		record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) ||
		record.Correlation.ProtocolOperationID != encodeCorrelationIdentity(operation.Bytes()) {
		t.Fatalf("capacity lifecycle record = %#v", record)
	}
}
