package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/engine/output"
)

func annotatedFailure(cause error, stage output.FailureStage) error {
	value, _ := output.SealFailure(cause, output.FailureDiagnostic{Stage: stage})
	return value
}

func TestRecoveryFailureRetainsActionableStagesAndOriginalCause(t *testing.T) {
	for _, test := range []struct {
		stage  output.FailureStage
		reason string
	}{
		{output.FailureDestinationBinding, "destination-binding-failed"},
		{output.FailureInventory, "inventory-paging-failed"},
		{output.FailureLookup, "active-lookup-failed"},
		{output.FailureOpen, "operation-acquisition-failed"},
		{output.FailureCreate, "operation-admission-failed"},
		{output.FailureCheckpoint, "checkpoint-reconciliation-failed"},
		{output.FailureDurability, "output-durability-failed"},
		{output.FailureClose, "authority-close-failed"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			cause := annotatedFailure(errors.New("provider unavailable"), test.stage)
			failure := DestinationFailure(cause)
			if failure.Kind != FailureNeedsAttention || failure.Reason != test.reason ||
				failure.Detail.Stage != test.stage || !failure.Detail.Valid() ||
				!errors.Is(failure, cause) || failure.Error() != test.reason {
				t.Fatalf("failure=%+v cause=%v", failure, cause)
			}
		})
	}
	if failure := DestinationFailure(errors.New("unclassified provider")); failure.Detail != (FailureDetail{}) {
		t.Fatalf("unclassified cause invented a stage: %+v", failure)
	}
}
func TestRecoveryFailureDetailRejectsUnknownStage(t *testing.T) {
	if (FailureDetail{Stage: 255}).Valid() || !(FailureDetail{}).Valid() {
		t.Fatal("invalid optional failure stage")
	}
}

func TestDestinationAndOperationContentionRemainDistinct(t *testing.T) {
	destination := DestinationFailure(output.ErrBusy)
	operation := classifyFailure(output.ErrBusy, true)
	if destination.Kind != FailureBusy || operation.Kind != FailureBusy ||
		destination.Reason == operation.Reason {
		t.Fatalf("lock scopes conflated: destination=%+v operation=%+v", destination, operation)
	}
	failure := DestinationFailure(errors.Join(context.Canceled, output.ErrBusy))
	if failure.Kind != FailureCancelled || !errors.Is(failure, output.ErrBusy) {
		t.Fatalf("cancellation precedence or retained cause lost: %+v", failure)
	}
}
