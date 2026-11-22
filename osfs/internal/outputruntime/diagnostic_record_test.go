package outputruntime

import (
	"math"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestDiagnosticRecordPreservesNativeFailureCapabilitiesAndIdentityScheme(t *testing.T) {
	yes := outputcap.SupportedCapability()
	capabilities, err := outputcap.NewDestinationCapabilities(yes, yes)
	if err != nil {
		t.Fatal(err)
	}
	source := Trace{
		Operation:                         TraceRuntimeDecision,
		IdentityScheme:                    outputcap.IdentitySchemePortableWitnessV1,
		RuntimeComponent:                  RuntimeFile,
		RuntimeOperation:                  RuntimeWriteRange,
		RuntimeDecision:                   RuntimeIsolatedFailure,
		Failed:                            true,
		FailureStage:                      FailureDurability,
		NativeErrorClass:                  outputcap.NativeErrorAccessDenied,
		ReconciliationStep:                CheckpointStageDurability,
		FaultDomain:                       1,
		NormalizedFaultScope:              2,
		NormalizedFaultCode:               3,
		ClaimID:                           math.MaxUint64,
		CheckpointRecordCount:             7,
		CheckpointReclaimedCandidateCount: 2,
		ReceiveOperationID:                receivecontract.OperationID{1},
		DestinationCapabilities:           capabilities,
	}
	record := source.DiagnosticRecord()
	p := record.Payload()
	if p.Get("runtime_operation").Text() != "write_range" ||
		p.Get("identity_scheme").Text() != string(outputcap.IdentitySchemePortableWitnessV1) ||
		p.Get("native_error_class").Text() != "access_denied" ||
		p.Get("failure_stage").Text() != "durability" ||
		p.Get("reconciliation_step").Text() != "stage_durability" ||
		p.Get("checkpoint_reclaimed_candidate_count").Uint() != 2 ||
		p.Get("claim_id").Uint() != math.MaxUint64 ||
		p.Get("fault").Get("code").Uint() != 3 ||
		!p.Get("capabilities").Get("safe_publish").Get("supported").Bool() ||
		!p.Get("capabilities").Get("operation_recovery").Get("supported").Bool() ||
		record.Correlation().Get("receive_operation_id").Text() != diagnostics.Identity(source.ReceiveOperationID.Bytes()).Text() {
		t.Fatalf("native evidence lost: %+v", p)
	}
	source.RuntimeOperation = RuntimeOperation(255)
	if source.DiagnosticRecord().Payload().Get("runtime_operation").Text() != "unknown_255" {
		t.Fatal("new native operation discarded")
	}

	mismatch := Trace{
		Operation:            TraceRuntimeDecision,
		DestinationBindingID: outputcap.DestinationBindingID{2},
		ReceiveOperationID:   receivecontract.OperationID{3},
		RuntimeComponent:     RuntimeSession,
		RuntimeOperation:     RuntimeSelectOperation,
		RuntimeDecision:      RuntimeSelectionMismatch,
	}.DiagnosticRecord()
	mp := mismatch.Payload()
	if mp.Get("runtime_operation").Text() != "select_operation" ||
		mp.Get("runtime_decision").Text() != "selection_mismatch" ||
		mp.Get("capabilities").Present() {
		t.Fatalf("selection mismatch diagnostic = %+v", mp)
	}
}
