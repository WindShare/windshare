package wire_test

import (
	"testing"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestProgressPayloadRecordsRawCapacityWaitIndependentOfPresentation(t *testing.T) {
	for _, visible := range []bool{false, true} {
		snapshot, err := appobs.NewProgressSnapshot(appobs.ProgressSpec{
			Discovery: appobs.DiscoveryComplete, CountersExact: true,
			CapacityActiveWaiters: 2, CapacityAccumulatedWait: 1250 * time.Millisecond,
			CapacityWaitAttempts: 3, CapacityWaitVisible: visible,
		})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := wire.EncodeProgress(snapshot)
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
	receive, _ := appobs.NewReceiveOperationID(identityBytes(0x11))
	session, _ := appobs.NewProtocolSessionID(identityBytes(0x12))
	job, _ := appobs.NewTransferJobID(identityBytes(0x13))
	wait, _ := appobs.NewCapacityWaitID(identityBytes(0x14))
	generation, _ := appobs.NewCapacityGenerationID(identityBytes(0x15))
	operation, _ := appobs.NewProtocolOperationID(identityBytes(0x16))
	progress, err := appobs.NewProgressSnapshot(appobs.ProgressSpec{
		Discovery: appobs.DiscoveryOpen, CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := appobs.NewTransferLifecycleObserved(appobs.TransferLifecycleSpec{
		ReceiveOperation: receive, ProtocolSession: session, TransferJob: job,
		Stage: appobs.TransferCapacityRetryScheduled, Progress: progress,
		FileSelection: appobs.FileSelectionNone, FileSettlement: appobs.FileSettlementNone,
		ItemBlockReason: appobs.ItemBlockNone, TreeSettlement: appobs.TreeSettlementNone,
		CapacityWait: wait, CapacityGeneration: generation, CapacityOperation: operation,
		CapacityAttempt: 2, CapacityHint: 250 * time.Millisecond,
		CapacityJitter: 9 * time.Millisecond, CapacityDelay: 259 * time.Millisecond,
		CapacityAccumulatedWait: 400 * time.Millisecond, CapacityActiveWaiters: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, encodeErr := wire.Encode(event)
	if err := encodeErr; err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(wire.TransferLifecyclePayload)
	if !ok || payload.Capacity == nil || payload.Capacity.WaitID != wire.EncodeIdentity(wait.Bytes()) ||
		payload.Capacity.GenerationID != wire.EncodeIdentity(generation.Bytes()) ||
		payload.Capacity.ProtocolOperationID != wire.EncodeIdentity(operation.Bytes()) ||
		payload.Capacity.Attempt != "2" || payload.Capacity.HintMS != "250" ||
		payload.Capacity.JitterMS != "9" || payload.Capacity.DelayMS != "259" ||
		payload.Capacity.AccumulatedWaitMS != "400" || payload.Capacity.ActiveWaiters != 3 ||
		record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != wire.EncodeIdentity(session.Bytes()) ||
		record.Correlation.ProtocolOperationID != wire.EncodeIdentity(operation.Bytes()) {
		t.Fatalf("capacity lifecycle record = %#v", record)
	}
}
