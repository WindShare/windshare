package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

type recoveryDiagnosticError struct {
	diagnostic osfs.FilesystemOutputDiagnostic
}

func (failure recoveryDiagnosticError) Error() string { return "native authority failure" }
func (failure recoveryDiagnosticError) FilesystemOutputDiagnostic() osfs.FilesystemOutputDiagnostic {
	return failure.diagnostic
}

func TestRecoveryFailureRetainsActionableNativeStagesAndOriginalCause(t *testing.T) {
	for _, test := range []struct {
		stage  osfs.FilesystemOutputFailureStage
		reason string
	}{
		{osfs.FilesystemOutputFailureDestinationBinding, "destination-binding-failed"},
		{osfs.FilesystemOutputFailureInventoryPaging, "inventory-paging-failed"},
		{osfs.FilesystemOutputFailureActiveLookup, "active-lookup-failed"},
		{osfs.FilesystemOutputFailureOperationAcquisition, "operation-acquisition-failed"},
		{osfs.FilesystemOutputFailureOperationAdmission, "operation-admission-failed"},
		{osfs.FilesystemOutputFailureCheckpointReconciliation, "checkpoint-reconciliation-failed"},
		{osfs.FilesystemOutputFailureNativeDurability, "native-durability-failed"},
		{osfs.FilesystemOutputFailureAuthorityClose, "authority-close-failed"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			cause := recoveryDiagnosticError{diagnostic: osfs.FilesystemOutputDiagnostic{Stage: test.stage}}
			failure := DestinationFailure(cause)
			if failure.Kind != FailureNeedsAttention || failure.Reason != test.reason ||
				failure.Detail.Stage != test.stage || !failure.Detail.Valid() ||
				!errors.Is(failure, cause) || failure.Error() != test.reason {
				t.Fatalf("failure=%+v cause=%v", failure, cause)
			}
		})
	}
	cause := recoveryDiagnosticError{diagnostic: osfs.FilesystemOutputDiagnostic{
		Stage:              osfs.FilesystemOutputFailureNativeDurability,
		ReconciliationStep: osfs.FilesystemCheckpointStageDurability,
		NativeErrorClass:   osfs.FilesystemNativeErrorAccessDenied,
	}}
	failure := DestinationFailure(cause)
	if failure.Detail.Reconciliation != cause.diagnostic.ReconciliationStep ||
		failure.Detail.NativeClass != cause.diagnostic.NativeErrorClass || !failure.Detail.Valid() {
		t.Fatalf("diagnostic lost recovery context: %+v", failure.Detail)
	}
	if failure := DestinationFailure(recoveryDiagnosticError{}); failure.Detail != (FailureDetail{}) {
		t.Fatalf("invalid diagnostic accepted: %+v", failure)
	}
}

func TestRecoveryFailureDetailRejectsImpossibleReconciliationAndNativeClasses(t *testing.T) {
	for _, invalid := range []FailureDetail{
		{Reconciliation: osfs.FilesystemCheckpointStageDurability},
		{Stage: osfs.FilesystemOutputFailureDestinationBinding, Reconciliation: osfs.FilesystemCheckpointStageDurability},
		{Stage: osfs.FilesystemOutputFailureCheckpointReconciliation, Reconciliation: 255},
		{Stage: osfs.FilesystemOutputFailureCheckpointReconciliation, NativeClass: 255},
	} {
		if invalid.Valid() {
			t.Fatalf("impossible detail accepted: %+v", invalid)
		}
	}
	if !(FailureDetail{}).Valid() {
		t.Fatal("absent diagnostics were rejected")
	}
}

func TestDestinationAndOperationContentionRemainDistinct(t *testing.T) {
	destination := DestinationFailure(osfs.ErrResumeStateBusy)
	operation := classifyFailure(osfs.ErrResumeStateBusy, true)
	if destination.Kind != FailureBusy || operation.Kind != FailureBusy ||
		destination.Reason == operation.Reason {
		t.Fatalf("lock scopes conflated: destination=%+v operation=%+v", destination, operation)
	}
	failure := DestinationFailure(errors.Join(context.Canceled, osfs.ErrResumeStateBusy))
	if failure.Kind != FailureCancelled || !errors.Is(failure, osfs.ErrResumeStateBusy) {
		t.Fatalf("cancellation precedence or retained cause lost: %+v", failure)
	}
}
