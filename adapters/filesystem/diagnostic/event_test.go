package diagnostic

import (
	"bytes"
	"errors"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
)

func TestFilesystemOutputEventPreservesRecoveryFailureClassification(t *testing.T) {
	receiveOperation, err := appobs.NewReceiveOperationID(bytes16(20))
	if err != nil {
		t.Fatal(err)
	}
	outputSession, err := appobs.NewOutputSessionID(bytes16(40))
	if err != nil {
		t.Fatal(err)
	}
	receiveIntent, err := appobs.NewReceiveIntentDigest(bytes.Repeat([]byte{0x5a}, appobs.ReceiveIntentDigestBytes))
	if err != nil {
		t.Fatal(err)
	}
	failure, err := appobs.NewFailure(appobs.FailureCheckpointStateIO)
	if err != nil {
		t.Fatal(err)
	}

	spec := FilesystemOutputSpec{
		Operation:           FilesystemRuntimeDecision,
		ReceiveIntent:       receiveIntent,
		ReceiveOperation:    receiveOperation,
		OutputSession:       outputSession,
		Certification:       FilesystemCertificationWindowsNTFSProcessRestart,
		NativeLockScope:     FilesystemNativeLockSession,
		NativeLockMilestone: FilesystemNativeLockAcquired,
		RootDisposition:     FilesystemRootCallerProvidedContainer,
		RuntimeComponent:    FilesystemRuntimeCheckpoint,
		RuntimeOperation:    FilesystemRuntimeReconcileCheckpoints,
		RuntimeDecision:     FilesystemRuntimeNeedsAttention,
		OperationID:         17,
		ClaimID:             23,
		Counters:            FilesystemOutputCounters{CheckpointRecords: 1, ActiveFileClaims: 1},
		Failure:             failure,
		FailureStage:        FilesystemFailureNativeDurability,
		ReconciliationStep:  FilesystemReconciliationStageDurability,
		NativeErrorClass:    FilesystemNativeErrorAccessDenied,
	}
	event, err := NewFilesystemOutputObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	if event.Workflow() != appobs.WorkflowReceive || event.Level() != appobs.LevelDebug || event.Operation() != spec.Operation {
		t.Fatalf("event envelope = command %d level %d operation %d", event.Workflow(), event.Level(), event.Operation())
	}
	if got, ok := event.ReceiveOperationID(); !ok || got != receiveOperation {
		t.Fatalf("receive operation = %+v,%t", got, ok)
	}
	if got, ok := event.ReceiveIntentDigest(); !ok || got != receiveIntent {
		t.Fatalf("receive intent = %+v,%t", got, ok)
	}
	if got, ok := event.OutputSessionID(); !ok || got != outputSession {
		t.Fatalf("output session = %+v,%t", got, ok)
	}
	if got, ok := event.Certification(); !ok || got != spec.Certification {
		t.Fatalf("certification = %d,%t", got, ok)
	}
	if scope, milestone, ok := event.NativeLock(); !ok || scope != spec.NativeLockScope || milestone != spec.NativeLockMilestone {
		t.Fatalf("native lock = %d/%d,%t", scope, milestone, ok)
	}
	if got, ok := event.RootDisposition(); !ok || got != spec.RootDisposition {
		t.Fatalf("root disposition = %d,%t", got, ok)
	}
	if component, operation, decision, ok := event.RuntimeDecision(); !ok ||
		component != spec.RuntimeComponent || operation != spec.RuntimeOperation || decision != spec.RuntimeDecision {
		t.Fatalf("runtime decision = %d/%d/%d,%t", component, operation, decision, ok)
	}
	if operationID, claimID := event.Correlation(); operationID != spec.OperationID || claimID != spec.ClaimID {
		t.Fatalf("correlation = %d/%d", operationID, claimID)
	}
	if got := event.Counters(); got != spec.Counters {
		t.Fatalf("counters = %+v", got)
	}
	if got, ok := event.Failure(); !ok || got.Code() != appobs.FailureCheckpointStateIO {
		t.Fatalf("failure = %+v,%t", got, ok)
	}
	if stage, step, nativeClass, ok := event.FailureClassification(); !ok ||
		stage != spec.FailureStage || step != spec.ReconciliationStep || nativeClass != spec.NativeErrorClass {
		t.Fatalf("failure classification = %d/%d/%d,%t", stage, step, nativeClass, ok)
	}

	tests := []struct {
		name   string
		mutate func(*FilesystemOutputSpec)
	}{
		{"unknown operation", func(value *FilesystemOutputSpec) { value.Operation = 0 }},
		{"unknown certification", func(value *FilesystemOutputSpec) { value.Certification = 255 }},
		{"unpaired native lock", func(value *FilesystemOutputSpec) { value.NativeLockScope = 0 }},
		{"partial runtime decision", func(value *FilesystemOutputSpec) { value.RuntimeDecision = 0 }},
		{"failure without stage", func(value *FilesystemOutputSpec) { value.FailureStage = 0 }},
		{"stage without failure", func(value *FilesystemOutputSpec) { value.Failure = appobs.Failure{} }},
		{"reconciliation outside recovery", func(value *FilesystemOutputSpec) {
			value.FailureStage = FilesystemFailureDestinationBinding
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := spec
			test.mutate(&invalid)
			if _, err := NewFilesystemOutputObserved(invalid); !errors.Is(err, appobs.ErrInvalidEvent) {
				t.Fatalf("invalid filesystem event error = %v", err)
			}
		})
	}
}

func TestFilesystemRuntimeFailureRequiresCompleteOperationContext(t *testing.T) {
	failure, err := appobs.NewFailure(appobs.FailureSessionDependencyContract)
	if err != nil {
		t.Fatal(err)
	}
	spec := FilesystemOutputSpec{
		Operation: FilesystemRuntimeDecision, Failure: failure,
		RuntimeComponent: FilesystemRuntimeSession,
		RuntimeOperation: FilesystemRuntimeFinalizeTree,
		RuntimeDecision:  FilesystemRuntimeClosed,
	}
	event, err := NewFilesystemOutputObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := event.Failure(); !ok || got != failure {
		t.Fatalf("runtime failure = (%v, %t)", got, ok)
	}
	if _, _, _, classified := event.FailureClassification(); classified {
		t.Fatal("runtime failure fabricated a native I/O classification")
	}
	for _, test := range []struct {
		name   string
		mutate func(*FilesystemOutputSpec)
	}{
		{"non-runtime event", func(value *FilesystemOutputSpec) { value.Operation = FilesystemSessionOpened }},
		{"missing component", func(value *FilesystemOutputSpec) { value.RuntimeComponent = 0 }},
		{"missing operation", func(value *FilesystemOutputSpec) { value.RuntimeOperation = 0 }},
		{"missing decision", func(value *FilesystemOutputSpec) { value.RuntimeDecision = 0 }},
		{"unknown operation", func(value *FilesystemOutputSpec) { value.RuntimeOperation = 255 }},
		{"unclassified native error", func(value *FilesystemOutputSpec) {
			value.NativeErrorClass = FilesystemNativeErrorAccessDenied
		}},
		{"unclassified reconciliation", func(value *FilesystemOutputSpec) {
			value.ReconciliationStep = FilesystemReconciliationStageDurability
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := spec
			test.mutate(&invalid)
			if _, err := NewFilesystemOutputObserved(invalid); !errors.Is(err, appobs.ErrInvalidEvent) {
				t.Fatalf("incomplete failure context accepted: %v", err)
			}
		})
	}
}

func bytes16(value byte) []byte { return bytes.Repeat([]byte{value}, 16) }
