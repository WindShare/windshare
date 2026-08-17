package outputruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type outcomeNativeFailure struct {
	class outputcap.NativeErrorClass
	text  string
}

func (failure outcomeNativeFailure) Error() string { return failure.text }
func (failure outcomeNativeFailure) NativeErrorClass() outputcap.NativeErrorClass {
	return failure.class
}

func TestFilesystemOutputDiagnosticFreezesPrimaryBeforeCleanupJoin(t *testing.T) {
	primaryCanary := outcomeNativeFailure{
		class: outputcap.NativeErrorAccessDenied,
		text:  "primary provider path content capability canary",
	}
	cleanupCanary := outcomeNativeFailure{
		class: outputcap.NativeErrorSharingViolation,
		text:  "cleanup provider path content capability canary",
	}
	primary := diagnoseFilesystemOutputFailure(
		FilesystemOutputFailureNativeDurability,
		primaryCanary,
	)
	joined := freezeFilesystemOutputFailure(primary, cleanupCanary)
	diagnostic, ok := FilesystemOutputDiagnosticFor(joined)
	if !ok || diagnostic.Stage != FilesystemOutputFailureNativeDurability ||
		diagnostic.NativeErrorClass != FilesystemNativeErrorAccessDenied {
		t.Fatalf("joined diagnostic = (%+v, %t)", diagnostic, ok)
	}
	if !errors.Is(joined, primaryCanary) || !errors.Is(joined, cleanupCanary) {
		t.Fatalf("joined cause identity was lost: %v", joined)
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

func TestFilesystemOutputClosedVocabulariesRejectUnknownValues(t *testing.T) {
	if FilesystemOutputFailureStage(0).Valid() ||
		FilesystemOutputFailureStage(255).Valid() ||
		FilesystemCheckpointReconciliationStep(0).Valid() ||
		FilesystemCheckpointReconciliationStep(255).Valid() ||
		FilesystemNativeErrorClass(0).Valid() ||
		FilesystemNativeErrorClass(255).Valid() ||
		FilesystemOutputStateReason(0).Valid() ||
		FilesystemOutputStateReason(255).Valid() {
		t.Fatal("unknown filesystem outcome value escaped its closed vocabulary")
	}
}

func TestFilesystemOutputVocabularyPreservesRecoveryDecisions(t *testing.T) {
	for _, test := range []struct {
		value FilesystemOutputFailureStage
		text  string
	}{
		{FilesystemOutputFailureDestinationBinding, "destination_binding"},
		{FilesystemOutputFailureInventoryPaging, "inventory_paging"},
		{FilesystemOutputFailureActiveLookup, "active_lookup"},
		{FilesystemOutputFailureOperationAcquisition, "operation_acquisition"},
		{FilesystemOutputFailureOperationAdmission, "operation_admission"},
		{FilesystemOutputFailureCheckpointReconciliation, "checkpoint_reconciliation"},
		{FilesystemOutputFailureNativeDurability, "native_durability"},
		{FilesystemOutputFailureAuthorityClose, "authority_close"},
	} {
		if got := test.value.String(); got != test.text {
			t.Fatalf("failure stage %d = %q, want %q", test.value, got, test.text)
		}
	}
	if got := FilesystemOutputFailureStage(255).String(); got != "" {
		t.Fatalf("unknown failure stage = %q", got)
	}

	for _, test := range []struct {
		source checkpointstore.ReconciliationStep
		value  FilesystemCheckpointReconciliationStep
		text   string
	}{
		{checkpointstore.ReconciliationCandidateObservation, FilesystemCheckpointCandidateObservation, "candidate_observation"},
		{checkpointstore.ReconciliationStageDurability, FilesystemCheckpointStageDurability, "stage_durability"},
		{checkpointstore.ReconciliationNamespaceDurability, FilesystemCheckpointNamespaceDurability, "namespace_durability"},
		{checkpointstore.ReconciliationRecordPromotion, FilesystemCheckpointRecordPromotion, "record_promotion"},
	} {
		if got := filesystemReconciliationStep(test.source); got != test.value || got.String() != test.text {
			t.Fatalf("reconciliation step %d = (%d, %q)", test.source, got, got.String())
		}
	}
	if got := filesystemReconciliationStep(255); got != 0 || got.String() != "" {
		t.Fatalf("unknown reconciliation step = (%d, %q)", got, got.String())
	}

	for _, test := range []struct {
		source outputcap.NativeErrorClass
		value  FilesystemNativeErrorClass
		text   string
	}{
		{outputcap.NativeErrorAccessDenied, FilesystemNativeErrorAccessDenied, "access_denied"},
		{outputcap.NativeErrorSharingViolation, FilesystemNativeErrorSharingViolation, "sharing_violation"},
		{outputcap.NativeErrorNotFound, FilesystemNativeErrorNotFound, "not_found"},
		{outputcap.NativeErrorInvalidHandle, FilesystemNativeErrorInvalidHandle, "invalid_handle"},
		{outputcap.NativeErrorUnsupported, FilesystemNativeErrorUnsupported, "unsupported"},
		{outputcap.NativeErrorIO, FilesystemNativeErrorIO, "io"},
		{outputcap.NativeErrorUnknown, FilesystemNativeErrorUnknown, "unknown"},
	} {
		if got := filesystemNativeErrorClass(test.source); got != test.value || got.String() != test.text {
			t.Fatalf("native class %d = (%d, %q)", test.source, got, got.String())
		}
	}
	if got := filesystemNativeErrorClass(255); got != 0 || got.String() != "" {
		t.Fatalf("unknown native class = (%d, %q)", got, got.String())
	}

	for _, test := range []struct {
		source checkpointmodel.OrdinaryClosedReason
		value  FilesystemOutputStateReason
		text   string
	}{
		{checkpointmodel.OrdinaryReasonNone, FilesystemOutputStateReasonNone, "none"},
		{checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown, FilesystemOutputStateDestinationOwnershipUnknown, "destination_ownership_unknown"},
		{checkpointmodel.OrdinaryReasonRegistryOwnershipUnknown, FilesystemOutputStateRegistryOwnershipUnknown, "registry_ownership_unknown"},
		{checkpointmodel.OrdinaryReasonLeaseOwnershipUnknown, FilesystemOutputStateLeaseOwnershipUnknown, "lease_ownership_unknown"},
		{checkpointmodel.OrdinaryReasonOperationOwnershipUnknown, FilesystemOutputStateOperationOwnershipUnknown, "operation_ownership_unknown"},
		{checkpointmodel.OrdinaryReasonCleanupUncertain, FilesystemOutputStateCleanupUncertain, "cleanup_uncertain"},
	} {
		if got := filesystemOutputStateReason(test.source); got != test.value || got.String() != test.text {
			t.Fatalf("state reason %d = (%d, %q)", test.source, got, got.String())
		}
	}
	if got := filesystemOutputStateReason(255); got != 0 || got.String() != "" {
		t.Fatalf("unknown state reason = (%d, %q)", got, got.String())
	}
}

func TestDiagnoseFilesystemOutputFailurePreservesFirstSafeEvidence(t *testing.T) {
	providerFailure := outcomeNativeFailure{
		class: outputcap.NativeErrorAccessDenied,
		text:  "provider path content capability canary",
	}
	diagnosed := DiagnoseFilesystemOutputFailure(
		FilesystemOutputFailureNativeDurability,
		providerFailure,
	)
	diagnostic, ok := FilesystemOutputDiagnosticFor(diagnosed)
	if !ok || diagnostic.Stage != FilesystemOutputFailureNativeDurability ||
		diagnostic.NativeErrorClass != FilesystemNativeErrorAccessDenied ||
		!errors.Is(diagnosed, providerFailure) {
		t.Fatalf("native durability diagnosis = (%+v, %t, %v)", diagnostic, ok, diagnosed)
	}

	rediagnosed := DiagnoseFilesystemOutputFailure(
		FilesystemOutputFailureDestinationBinding,
		diagnosed,
	)
	retained, ok := FilesystemOutputDiagnosticFor(rediagnosed)
	if !ok || retained != diagnostic || !errors.Is(rediagnosed, providerFailure) {
		t.Fatalf("rediagnosed failure replaced first evidence = (%+v, %t, %v)", retained, ok, rediagnosed)
	}

	cleanupFailure := errors.New("cleanup failure canary")
	closeOnly := freezeFilesystemOutputFailure(nil, cleanupFailure)
	closeDiagnostic, ok := FilesystemOutputDiagnosticFor(closeOnly)
	if !ok || closeDiagnostic.Stage != FilesystemOutputFailureAuthorityClose ||
		!errors.Is(closeOnly, cleanupFailure) {
		t.Fatalf("close-only diagnosis = (%+v, %t, %v)", closeDiagnostic, ok, closeOnly)
	}
	primaryFailure := errors.New("primary failure canary")
	joined := freezeFilesystemOutputFailure(primaryFailure, cleanupFailure)
	if _, ok := FilesystemOutputDiagnosticFor(joined); ok ||
		!errors.Is(joined, primaryFailure) || !errors.Is(joined, cleanupFailure) {
		t.Fatalf("unclassified joined failure = %v", joined)
	}
	if got := freezeFilesystemOutputFailure(primaryFailure, nil); got != primaryFailure {
		t.Fatalf("nil cleanup changed primary identity: %v", got)
	}
	for _, canceled := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := DiagnoseFilesystemOutputFailure(FilesystemOutputFailureNativeDurability, canceled); got != canceled {
			t.Fatalf("cancellation identity changed: %v", got)
		}
	}
}

func TestFilesystemOutputDiagnosticRejectsPartialOrMisplacedEvidence(t *testing.T) {
	for _, diagnostic := range []FilesystemOutputDiagnostic{
		{},
		{Stage: FilesystemOutputFailureDestinationBinding, ReconciliationStep: 255},
		{Stage: FilesystemOutputFailureDestinationBinding, ReconciliationStep: FilesystemCheckpointCandidateObservation},
		{Stage: FilesystemOutputFailureDestinationBinding, NativeErrorClass: 255},
		{Stage: FilesystemOutputFailureDestinationBinding, FaultDomain: 1},
	} {
		if diagnostic.Valid() {
			t.Fatalf("invalid filesystem diagnostic was admitted: %+v", diagnostic)
		}
	}
	if diagnostic := (FilesystemOutputDiagnostic{
		Stage:              FilesystemOutputFailureCheckpointReconciliation,
		ReconciliationStep: FilesystemCheckpointCandidateObservation,
	}); !diagnostic.Valid() {
		t.Fatalf("complete reconciliation diagnostic was rejected: %+v", diagnostic)
	}
}

func TestFilesystemOutputDiagnosticNilCarrierFailsClosed(t *testing.T) {
	var failure *filesystemOutputDiagnosticError
	if got := failure.Error(); got != "filesystem output failed" {
		t.Fatalf("nil diagnostic error = %q", got)
	}
	if cause := failure.Unwrap(); cause != nil {
		t.Fatalf("nil diagnostic cause = %v", cause)
	}
	if diagnostic := failure.FilesystemOutputDiagnostic(); diagnostic != (FilesystemOutputDiagnostic{}) {
		t.Fatalf("nil diagnostic carrier = %+v", diagnostic)
	}
	if diagnostic, ok := FilesystemOutputDiagnosticFor(failure); ok || diagnostic != (FilesystemOutputDiagnostic{}) {
		t.Fatalf("nil diagnostic projection = (%+v, %t)", diagnostic, ok)
	}
}
