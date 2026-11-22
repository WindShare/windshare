package outputruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type outcomeNativeFailure struct {
	class outputcap.NativeErrorClass
	text  string
}

func (failure outcomeNativeFailure) Error() string { return failure.text }
func (failure outcomeNativeFailure) NativeErrorClass() outputcap.NativeErrorClass {
	return failure.class
}

func TestOutputDiagnosticFreezesPrimaryBeforeCleanupJoin(t *testing.T) {
	primaryCanary := outcomeNativeFailure{
		class: outputcap.NativeErrorAccessDenied,
		text:  "primary provider path content capability canary",
	}
	cleanupCanary := outcomeNativeFailure{
		class: outputcap.NativeErrorSharingViolation,
		text:  "cleanup provider path content capability canary",
	}
	primary := DiagnoseFailure(FailureDurability, primaryCanary)
	joined := freezeFailure(primary, cleanupCanary)
	diagnostic, ok := DiagnosticFor(joined)
	if !ok || diagnostic.Stage != FailureDurability ||
		diagnostic.NativeErrorClass != outputcap.NativeErrorAccessDenied {
		t.Fatalf("joined diagnostic = (%+v, %t)", diagnostic, ok)
	}
	if !errors.Is(joined, primaryCanary) || !errors.Is(joined, cleanupCanary) {
		t.Fatalf("joined cause identity was lost: %v", joined)
	}
	cleanup := FailureForStage(joined, FailureClose)
	cleanupDiagnostic, ok := DiagnosticFor(cleanup)
	if !ok || cleanupDiagnostic.Stage != FailureClose ||
		cleanupDiagnostic.NativeErrorClass != outputcap.NativeErrorSharingViolation ||
		!errors.Is(cleanup, cleanupCanary) || errors.Is(cleanup, primaryCanary) {
		t.Fatalf("cleanup diagnostic was hidden behind primary: (%+v, %v)", cleanupDiagnostic, cleanup)
	}
	for _, safe := range []string{
		diagnostic.Stage.String(),
		diagnostic.NativeErrorClass.String(),
		joined.Error(),
	} {
		if strings.Contains(safe, "provider") || strings.Contains(safe, "path") ||
			strings.Contains(safe, "content") || strings.Contains(safe, "capability") {
			t.Fatalf("public diagnostic leaked provider evidence: %q", safe)
		}
	}
}

func TestOutputClosedVocabulariesRejectUnknownValues(t *testing.T) {
	if FailureStage(0).Valid() || FailureStage(255).Valid() ||
		CheckpointReconciliationStep(0).Valid() ||
		CheckpointReconciliationStep(255).Valid() ||
		NativeErrorClass(0).Valid() ||
		NativeErrorClass(255).Valid() {
		t.Fatal("unknown filesystem outcome value escaped its closed vocabulary")
	}
}

func TestOutputVocabularyPreservesRecoveryDecisions(t *testing.T) {
	for _, test := range []struct {
		source checkpointstore.ReconciliationStep
		value  CheckpointReconciliationStep
		text   string
	}{
		{checkpointstore.ReconciliationCandidateObservation, CheckpointCandidateObservation, "candidate_observation"},
		{checkpointstore.ReconciliationStageDurability, CheckpointStageDurability, "stage_durability"},
		{checkpointstore.ReconciliationNamespaceDurability, CheckpointNamespaceDurability, "namespace_durability"},
		{checkpointstore.ReconciliationRecordPromotion, CheckpointRecordPromotion, "record_promotion"},
	} {
		if got := reconciliationStepFromStore(test.source); got != test.value || got.String() != test.text {
			t.Fatalf("reconciliation step %d = (%d, %q)", test.source, got, got.String())
		}
	}
	if got := reconciliationStepFromStore(255); got != 0 || got.String() != "" {
		t.Fatalf("unknown reconciliation step = (%d, %q)", got, got.String())
	}

	for _, test := range []struct {
		source checkpointmodel.OrdinaryClosedReason
		value  AttentionReason
	}{
		{checkpointmodel.OrdinaryReasonNone, AttentionNone},
		{checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown, AttentionDestination},
		{checkpointmodel.OrdinaryReasonRegistryOwnershipUnknown, AttentionRegistry},
		{checkpointmodel.OrdinaryReasonLeaseOwnershipUnknown, AttentionLease},
		{checkpointmodel.OrdinaryReasonOperationOwnershipUnknown, AttentionOperation},
		{checkpointmodel.OrdinaryReasonCleanupUncertain, AttentionCleanup},
	} {
		if got := attentionReasonFromClosed(test.source); got != test.value {
			t.Fatalf("attention reason %d = %q, want %q", test.source, got, test.value)
		}
	}
	if got := attentionReasonFromClosed(255); got != "" {
		t.Fatalf("unknown attention reason = %q", got)
	}
}

func TestDiagnoseFailurePreservesFirstSafeEvidence(t *testing.T) {
	providerFailure := outcomeNativeFailure{
		class: outputcap.NativeErrorAccessDenied,
		text:  "provider path content capability canary",
	}
	diagnosed := DiagnoseFailure(FailureDurability, providerFailure)
	diagnostic, ok := DiagnosticFor(diagnosed)
	if !ok || diagnostic.Stage != FailureDurability ||
		diagnostic.NativeErrorClass != outputcap.NativeErrorAccessDenied ||
		!errors.Is(diagnosed, providerFailure) {
		t.Fatalf("native durability diagnosis = (%+v, %t, %v)", diagnostic, ok, diagnosed)
	}

	rediagnosed := DiagnoseFailure(FailureDestinationBinding, diagnosed)
	retained, ok := DiagnosticFor(rediagnosed)
	if !ok || retained != diagnostic || !errors.Is(rediagnosed, providerFailure) {
		t.Fatalf("rediagnosed failure replaced first evidence = (%+v, %t, %v)", retained, ok, rediagnosed)
	}

	cleanupFailure := errors.New("cleanup failure canary")
	closeOnly := freezeFailure(nil, cleanupFailure)
	closeDiagnostic, ok := DiagnosticFor(closeOnly)
	if !ok || closeDiagnostic.Stage != FailureClose ||
		!errors.Is(closeOnly, cleanupFailure) {
		t.Fatalf("close-only diagnosis = (%+v, %t, %v)", closeDiagnostic, ok, closeOnly)
	}
	primaryFailure := errors.New("primary failure canary")
	joined := freezeFailure(primaryFailure, cleanupFailure)
	if diagnostic, ok := DiagnosticFor(joined); !ok || diagnostic.Stage != FailureClose ||
		!errors.Is(joined, primaryFailure) || !errors.Is(joined, cleanupFailure) {
		t.Fatalf("joined failure lost independently classified cleanup = %v", joined)
	}
	if got := freezeFailure(primaryFailure, nil); got != primaryFailure {
		t.Fatalf("nil cleanup changed primary identity: %v", got)
	}
	for _, canceled := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := DiagnoseFailure(FailureDurability, canceled); got != canceled {
			t.Fatalf("cancellation identity changed: %v", got)
		}
	}
}

func TestOutputDiagnosticRejectsPartialOrMisplacedEvidence(t *testing.T) {
	for _, diagnostic := range []Diagnostic{
		{},
		{Stage: FailureDestinationBinding, ReconciliationStep: 255},
		{Stage: FailureDestinationBinding, ReconciliationStep: CheckpointCandidateObservation},
		{Stage: FailureDestinationBinding, NativeErrorClass: 255},
		{Stage: FailureDestinationBinding, FaultDomain: 1},
	} {
		if diagnostic.Valid() {
			t.Fatalf("invalid diagnostic was admitted: %+v", diagnostic)
		}
	}
	if diagnostic := (Diagnostic{
		Stage:              FailureCheckpoint,
		ReconciliationStep: CheckpointCandidateObservation,
	}); !diagnostic.Valid() {
		t.Fatalf("complete reconciliation diagnostic was rejected: %+v", diagnostic)
	}
}

func TestOutputDiagnosticNilCarrierFailsClosed(t *testing.T) {
	var failure *diagnosticError
	if got := failure.Error(); got != "filesystem output failed" {
		t.Fatalf("nil diagnostic error = %q", got)
	}
	if cause := failure.Unwrap(); cause != nil {
		t.Fatalf("nil diagnostic cause = %v", cause)
	}
	if diagnostic, ok := DiagnosticFor(failure); ok || diagnostic != (Diagnostic{}) {
		t.Fatalf("nil diagnostic projection = (%+v, %t)", diagnostic, ok)
	}
}
