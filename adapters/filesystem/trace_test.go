package filesystem

import (
	"errors"
	"testing"

	fsobs "github.com/windshare/windshare/adapters/filesystem/diagnostic"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/osfs"
)

func TestFilesystemProjectionRejectsUnknownValues(t *testing.T) {
	assertClosedProjection(t, "filesystem operation", []osfs.FilesystemOutputTraceOperation{
		osfs.TraceFilesystemCertified, osfs.TraceFeatureProbeCompleted,
		osfs.TraceCheckpointNamespaceOpened, osfs.TraceNativeLock,
		osfs.TraceSessionOpened, osfs.TraceCheckpointReconciled,
		osfs.TraceRuntimeDecision,
	}, osfs.FilesystemOutputTraceOperation(255), projectFilesystemOperation)
	assertClosedProjection(t, "filesystem checkpoint decision", []osfs.FilesystemCheckpointDecision{
		0, osfs.FilesystemCheckpointAbsent, osfs.FilesystemCheckpointExact,
		osfs.FilesystemCheckpointRevisionConflict, osfs.FilesystemCheckpointOwnershipConflict,
		osfs.FilesystemCheckpointInvalid,
	}, osfs.FilesystemCheckpointDecision(255), projectFilesystemCheckpointDecision)
}

func TestFilesystemCheckpointDecisionProjectionIsExact(t *testing.T) {
	tests := []struct {
		source osfs.FilesystemCheckpointDecision
		want   fsobs.FilesystemCheckpointDecision
	}{
		{osfs.FilesystemCheckpointAbsent, fsobs.FilesystemCheckpointAbsent},
		{osfs.FilesystemCheckpointExact, fsobs.FilesystemCheckpointExact},
		{osfs.FilesystemCheckpointRevisionConflict, fsobs.FilesystemCheckpointRevisionConflict},
		{osfs.FilesystemCheckpointOwnershipConflict, fsobs.FilesystemCheckpointOwnershipConflict},
		{osfs.FilesystemCheckpointInvalid, fsobs.FilesystemCheckpointInvalid},
	}
	for _, test := range tests {
		got, ok := projectFilesystemCheckpointDecision(test.source)
		if !ok || got != test.want {
			t.Fatalf("checkpoint decision %v projected as %v, present=%t", test.source, got, ok)
		}
	}
}

func assertClosedProjection[Source comparable, Target any](
	t *testing.T,
	name string,
	values []Source,
	unknown Source,
	project func(Source) (Target, bool),
) {
	t.Helper()
	for _, value := range values {
		if _, ok := project(value); !ok {
			t.Fatalf("%s rejected supported value %v", name, value)
		}
	}
	if _, ok := project(unknown); ok {
		t.Fatalf("%s accepted unknown value %v", name, unknown)
	}
}

func TestFilesystemFailureClassificationProjectionIsExhaustive(t *testing.T) {
	stages := []struct {
		source osfs.FilesystemOutputFailureStage
		want   fsobs.FilesystemFailureStage
	}{
		{osfs.FilesystemOutputFailureDestinationBinding, fsobs.FilesystemFailureDestinationBinding},
		{osfs.FilesystemOutputFailureInventoryPaging, fsobs.FilesystemFailureInventoryPaging},
		{osfs.FilesystemOutputFailureActiveLookup, fsobs.FilesystemFailureActiveLookup},
		{osfs.FilesystemOutputFailureOperationAcquisition, fsobs.FilesystemFailureOperationAcquisition},
		{osfs.FilesystemOutputFailureOperationAdmission, fsobs.FilesystemFailureOperationAdmission},
		{osfs.FilesystemOutputFailureCheckpointReconciliation, fsobs.FilesystemFailureCheckpointReconciliation},
		{osfs.FilesystemOutputFailureNativeDurability, fsobs.FilesystemFailureNativeDurability},
		{osfs.FilesystemOutputFailureAuthorityClose, fsobs.FilesystemFailureAuthorityClose},
	}
	for _, test := range stages {
		event, err := ProjectFilesystemOutput(osfs.FilesystemOutputTrace{
			Operation:    osfs.TraceRuntimeDecision,
			FailureStage: test.source,
			Failed:       true,
		})
		if err != nil {
			t.Fatalf("stage %v: %v", test.source, err)
		}
		stage, _, _, present := event.FailureClassification()
		if !present || stage != test.want {
			t.Fatalf("stage %v projected as %v, present=%t", test.source, stage, present)
		}
	}

	for name, source := range map[string]osfs.FilesystemOutputTrace{
		"unknown checkpoint decision": {
			Operation: osfs.TraceRuntimeDecision, CheckpointDecision: osfs.FilesystemCheckpointDecision(255),
		},
		"unknown stage": {
			Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureStage(255), Failed: true,
		},
		"unknown reconciliation step": {
			Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureNativeDurability,
			ReconciliationStep: osfs.FilesystemCheckpointReconciliationStep(255), Failed: true,
		},
		"unknown native class": {
			Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureNativeDurability,
			NativeErrorClass: osfs.FilesystemNativeErrorClass(255), Failed: true,
		},
	} {
		_, err := ProjectFilesystemOutput(source)
		var projection appobs.OutputDiagnosticError
		if !errors.As(err, &projection) || projection.Reason != appobs.ObserverLossUnknownEnum {
			t.Fatalf("%s error=%v", name, err)
		}
	}

	_, err := ProjectFilesystemOutput(osfs.FilesystemOutputTrace{
		Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureDestinationBinding,
		ReconciliationStep: osfs.FilesystemCheckpointRecordPromotion, Failed: true,
	})
	var projection appobs.OutputDiagnosticError
	if !errors.As(err, &projection) || projection.Reason != appobs.ObserverLossInvalidStageFields {
		t.Fatalf("invalid filesystem stage/step error=%v", err)
	}
}
