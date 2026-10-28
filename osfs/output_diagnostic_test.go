package osfs

import (
	"math"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestDiagnosticPreservesNativeFailureAndDestinationCapabilities(t *testing.T) {
	yes := outputcap.SupportedCapability()
	capabilities, err := outputcap.NewDestinationCapabilities(yes, yes, yes, yes)
	if err != nil {
		t.Fatal(err)
	}
	source := FilesystemOutputTrace{Operation: TraceRuntimeDecision, RuntimeComponent: FilesystemOutputRuntimeFile, RuntimeOperation: FilesystemOutputRuntimeWriteRange, RuntimeDecision: FilesystemOutputRuntimeIsolatedFailure,
		Failed: true, FailureStage: FilesystemOutputFailureNativeDurability, NativeErrorClass: FilesystemNativeErrorAccessDenied, ReconciliationStep: FilesystemCheckpointStageDurability,
		FaultDomain: 1, NormalizedFaultScope: 2, NormalizedFaultCode: 3, ClaimID: math.MaxUint64, CheckpointRecordCount: 7,
		ReceiveOperationID: receivecontract.OperationID{1}, DestinationCapabilities: capabilities, ExecutionMode: outputcap.ExecutionResumable}
	source.CheckpointReclaimedCandidateCount = 2
	record := source.DiagnosticRecord()
	p := record.Payload()
	if p.Get("runtime_operation").Text() != "write_range" || p.Get("native_error_class").Text() != "access_denied" || p.Get("failure_stage").Text() != "native_durability" || p.Get("reconciliation_step").Text() != "stage_durability" ||
		p.Get("checkpoint_reclaimed_candidate_count").Uint() != 2 || p.Get("claim_id").Uint() != math.MaxUint64 || p.Get("fault").Get("code").Uint() != 3 || !p.Get("capabilities").Get("range_recovery").Get("supported").Bool() ||
		record.Correlation().Get("receive_operation_id").Text() != diagnostics.Identity(source.ReceiveOperationID.Bytes()).Text() {
		t.Fatal("native evidence lost")
	}
	source.RuntimeOperation = FilesystemOutputRuntimeOperation(255)
	if source.DiagnosticRecord().Payload().Get("runtime_operation").Text() != "unknown_255" {
		t.Fatal("new native operation discarded")
	}
}
