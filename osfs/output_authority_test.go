package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputruntime"
)

func TestOutputAuthorityRejectsNilReceiver(t *testing.T) {
	var authority *OutputAuthority
	if err := authority.BindDestination(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority bind = %v, want invalid binding", err)
	}
	if _, err := authority.SelectOperation(
		context.Background(), transfer.SelectionSpec{}, receivecontract.AutomaticOperation(),
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority select = %v, want invalid binding", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("nil authority close = %v", err)
	}
	var operation *OutputOperation
	if _, err := operation.OpenDirectTree(context.Background(), transfer.ReceiveIntent{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil operation open = %v, want invalid binding", err)
	}
}

func TestOutputOutcomeVocabularyIsClosed(t *testing.T) {
	stages := []struct {
		value OutputFailureStage
		name  string
	}{
		{OutputFailureDestinationBinding, "destination_binding"},
		{OutputFailureInventory, "inventory"},
		{OutputFailureLookup, "lookup"},
		{OutputFailureOpen, "open"},
		{OutputFailureCreate, "create"},
		{OutputFailureCheckpoint, "checkpoint"},
		{OutputFailureDurability, "durability"},
		{OutputFailureClose, "close"},
	}
	for _, stage := range stages {
		if !stage.value.Valid() || stage.value.String() != stage.name {
			t.Fatalf("failure stage %d = %q, want %q", stage.value, stage.value, stage.name)
		}
	}

	reconciliationSteps := []struct {
		value CheckpointReconciliationStep
		name  string
	}{
		{CheckpointCandidateObservation, "candidate_observation"},
		{CheckpointStageDurability, "stage_durability"},
		{CheckpointNamespaceDurability, "namespace_durability"},
		{CheckpointRecordPromotion, "record_promotion"},
	}
	for _, step := range reconciliationSteps {
		if !step.value.Valid() || step.value.String() != step.name {
			t.Fatalf("reconciliation step %d = %q, want %q", step.value, step.value, step.name)
		}
	}

	nativeClasses := []struct {
		value NativeErrorClass
		name  string
	}{
		{NativeErrorAccessDenied, "access_denied"},
		{NativeErrorSharingViolation, "sharing_violation"},
		{NativeErrorNotFound, "not_found"},
		{NativeErrorInvalidHandle, "invalid_handle"},
		{NativeErrorUnsupported, "unsupported"},
		{NativeErrorIO, "io"},
		{NativeErrorUnknown, "unknown"},
	}
	for _, class := range nativeClasses {
		if !class.value.Valid() || class.value.String() != class.name {
			t.Fatalf("native error class %d = %q, want %q", class.value, class.value, class.name)
		}
	}

	if OutputFailureStage(0).Valid() || OutputFailureStage(255).Valid() ||
		CheckpointReconciliationStep(0).Valid() ||
		CheckpointReconciliationStep(255).Valid() ||
		NativeErrorClass(0).Valid() || NativeErrorClass(255).Valid() {
		t.Fatal("unknown native outcome value was accepted")
	}
	if OutputFailureStage(255).String() != "" ||
		CheckpointReconciliationStep(255).String() != "" ||
		NativeErrorClass(255).String() != "" {
		t.Fatal("native outcome vocabulary is not fail-closed")
	}
	if (OutputDiagnostic{
		Stage:              OutputFailureInventory,
		ReconciliationStep: CheckpointCandidateObservation,
	}).Valid() {
		t.Fatal("reconciliation evidence was accepted outside reconciliation or durability")
	}
	if !(OutputDiagnostic{
		Stage:              OutputFailureDurability,
		ReconciliationStep: CheckpointNamespaceDurability,
		NativeErrorClass:   NativeErrorSharingViolation,
	}).Valid() {
		t.Fatal("closed native diagnostic was rejected")
	}
}

func TestOutputDiagnosticForAdmitsOnlyCompleteBoundedEvidence(t *testing.T) {
	if (OutputDiagnostic{}).Valid() {
		t.Fatal("zero diagnostic was accepted")
	}
	if (OutputDiagnostic{
		Stage:           OutputFailureDurability,
		FaultDomain:     1,
		NormalizedScope: 2,
	}).Valid() {
		t.Fatal("incomplete normalized fault was accepted")
	}
	if (OutputDiagnostic{
		Stage:            OutputFailureDurability,
		NativeErrorClass: 255,
	}).Valid() {
		t.Fatal("unknown native error class was accepted")
	}
	if _, ok := OutputDiagnosticFor(errors.New("untyped provider failure")); ok {
		t.Fatal("untyped provider failure acquired diagnostic authority")
	}
	runtimeFailure := outputruntime.DiagnoseFailure(
		OutputFailureInventory,
		errors.New("registry paging failure"),
	)
	if diagnostic, ok := OutputDiagnosticFor(runtimeFailure); !ok ||
		diagnostic.Stage != OutputFailureInventory {
		t.Fatalf("runtime diagnostic projection = (%+v, %t)", diagnostic, ok)
	}
}
