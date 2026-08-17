package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
)

type filesystemOutputDiagnosticTestCarrier struct {
	diagnostic FilesystemOutputDiagnostic
}

func (carrier filesystemOutputDiagnosticTestCarrier) Error() string {
	return "filesystem diagnostic test carrier"
}

func (carrier filesystemOutputDiagnosticTestCarrier) FilesystemOutputDiagnostic() FilesystemOutputDiagnostic {
	return carrier.diagnostic
}

func TestFilesystemOutputAuthorityRejectsZeroAndNilCapabilities(t *testing.T) {
	var authority *FilesystemOutputAuthority
	if _, err := authority.OpenDirectTree(context.Background(), transfer.ReceiveIntent{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority open = %v, want invalid binding", err)
	}

	var trace FilesystemOutputTrace
	var nilTracer FilesystemOutputTraceFunc
	nilTracer.TraceFilesystemOutput(trace)
	called := false
	FilesystemOutputTraceFunc(func(FilesystemOutputTrace) { called = true }).TraceFilesystemOutput(trace)
	if !called {
		t.Fatal("non-nil trace function was not invoked")
	}
}

func TestFilesystemOutputOutcomeVocabularyIsClosed(t *testing.T) {
	stages := []struct {
		value FilesystemOutputFailureStage
		name  string
	}{
		{FilesystemOutputFailureDestinationBinding, "destination_binding"},
		{FilesystemOutputFailureInventoryPaging, "inventory_paging"},
		{FilesystemOutputFailureActiveLookup, "active_lookup"},
		{FilesystemOutputFailureOperationAcquisition, "operation_acquisition"},
		{FilesystemOutputFailureOperationAdmission, "operation_admission"},
		{FilesystemOutputFailureCheckpointReconciliation, "checkpoint_reconciliation"},
		{FilesystemOutputFailureNativeDurability, "native_durability"},
		{FilesystemOutputFailureAuthorityClose, "authority_close"},
	}
	for _, stage := range stages {
		if !stage.value.Valid() || stage.value.String() != stage.name {
			t.Fatalf("failure stage %d = %q, want %q", stage.value, stage.value, stage.name)
		}
	}

	reconciliationSteps := []struct {
		value FilesystemCheckpointReconciliationStep
		name  string
	}{
		{FilesystemCheckpointCandidateObservation, "candidate_observation"},
		{FilesystemCheckpointStageDurability, "stage_durability"},
		{FilesystemCheckpointNamespaceDurability, "namespace_durability"},
		{FilesystemCheckpointRecordPromotion, "record_promotion"},
	}
	for _, step := range reconciliationSteps {
		if !step.value.Valid() || step.value.String() != step.name {
			t.Fatalf("reconciliation step %d = %q, want %q", step.value, step.value, step.name)
		}
	}

	nativeClasses := []struct {
		value FilesystemNativeErrorClass
		name  string
	}{
		{FilesystemNativeErrorAccessDenied, "access_denied"},
		{FilesystemNativeErrorSharingViolation, "sharing_violation"},
		{FilesystemNativeErrorNotFound, "not_found"},
		{FilesystemNativeErrorInvalidHandle, "invalid_handle"},
		{FilesystemNativeErrorUnsupported, "unsupported"},
		{FilesystemNativeErrorIO, "io"},
		{FilesystemNativeErrorUnknown, "unknown"},
	}
	for _, class := range nativeClasses {
		if !class.value.Valid() || class.value.String() != class.name {
			t.Fatalf("native error class %d = %q, want %q", class.value, class.value, class.name)
		}
	}

	stateReasons := []struct {
		value FilesystemOutputStateReason
		name  string
	}{
		{FilesystemOutputStateReasonNone, "none"},
		{FilesystemOutputStateDestinationOwnershipUnknown, "destination-ownership-unknown"},
		{FilesystemOutputStateRegistryOwnershipUnknown, "registry-ownership-unknown"},
		{FilesystemOutputStateLeaseOwnershipUnknown, "lease-ownership-unknown"},
		{FilesystemOutputStateOperationOwnershipUnknown, "operation-ownership-unknown"},
		{FilesystemOutputStateCleanupUncertain, "cleanup-uncertain"},
	}
	for _, reason := range stateReasons {
		if !reason.value.Valid() || reason.value.String() != reason.name {
			t.Fatalf("output state reason %d = %q, want %q", reason.value, reason.value, reason.name)
		}
	}

	if FilesystemOutputFailureStage(0).Valid() || FilesystemOutputFailureStage(255).Valid() ||
		FilesystemCheckpointReconciliationStep(0).Valid() ||
		FilesystemCheckpointReconciliationStep(255).Valid() ||
		FilesystemNativeErrorClass(0).Valid() || FilesystemNativeErrorClass(255).Valid() ||
		FilesystemOutputStateReason(0).Valid() || FilesystemOutputStateReason(255).Valid() {
		t.Fatal("unknown filesystem outcome value was accepted")
	}
	if !FilesystemOutputStateReasonNone.Valid() ||
		FilesystemOutputFailureStage(255).String() != "" ||
		FilesystemCheckpointReconciliationStep(255).String() != "" ||
		FilesystemNativeErrorClass(255).String() != "" ||
		FilesystemOutputStateReason(255).String() != "" {
		t.Fatal("filesystem outcome vocabulary is not fail-closed")
	}
	if (FilesystemOutputDiagnostic{
		Stage:              FilesystemOutputFailureInventoryPaging,
		ReconciliationStep: FilesystemCheckpointCandidateObservation,
	}).Valid() {
		t.Fatal("reconciliation evidence was accepted outside reconciliation or durability")
	}
	if !(FilesystemOutputDiagnostic{
		Stage:              FilesystemOutputFailureNativeDurability,
		ReconciliationStep: FilesystemCheckpointNamespaceDurability,
		NativeErrorClass:   FilesystemNativeErrorSharingViolation,
	}).Valid() {
		t.Fatal("closed filesystem diagnostic was rejected")
	}
}

func TestFilesystemOutputDiagnosticForAdmitsOnlyCompleteBoundedEvidence(t *testing.T) {
	want := FilesystemOutputDiagnostic{
		Stage:              FilesystemOutputFailureNativeDurability,
		ReconciliationStep: FilesystemCheckpointStageDurability,
		NativeErrorClass:   FilesystemNativeErrorAccessDenied,
		FaultDomain:        1,
		NormalizedScope:    2,
		NormalizedCode:     3,
	}
	carried := errors.Join(errors.New("provider detail"), filesystemOutputDiagnosticTestCarrier{diagnostic: want})
	if got, ok := FilesystemOutputDiagnosticFor(carried); !ok || got != want {
		t.Fatalf("filesystem diagnostic = (%+v, %t), want (%+v, true)", got, ok, want)
	}

	malformed := filesystemOutputDiagnosticTestCarrier{diagnostic: FilesystemOutputDiagnostic{
		Stage:           FilesystemOutputFailureNativeDurability,
		FaultDomain:     1,
		NormalizedScope: 2,
	}}
	if _, ok := FilesystemOutputDiagnosticFor(malformed); ok {
		t.Fatal("incomplete normalized fault crossed the public diagnostic boundary")
	}
	if (FilesystemOutputDiagnostic{}).Valid() {
		t.Fatal("zero diagnostic was accepted")
	}
	if (FilesystemOutputDiagnostic{
		Stage:            FilesystemOutputFailureNativeDurability,
		NativeErrorClass: 255,
	}).Valid() {
		t.Fatal("unknown native error class was accepted")
	}
	if _, ok := FilesystemOutputDiagnosticFor(errors.New("untyped provider failure")); ok {
		t.Fatal("untyped provider failure acquired diagnostic authority")
	}
	runtimeFailure := outputruntime.DiagnoseFilesystemOutputFailure(
		outputruntime.FilesystemOutputFailureInventoryPaging,
		errors.New("registry paging failure"),
	)
	if diagnostic, ok := FilesystemOutputDiagnosticFor(runtimeFailure); !ok ||
		diagnostic.Stage != FilesystemOutputFailureInventoryPaging {
		t.Fatalf("runtime diagnostic projection = (%+v, %t)", diagnostic, ok)
	}
}
