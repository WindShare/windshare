package clievent

import (
	"bytes"
	"errors"
	"testing"
)

func TestFilesystemOutputEventPreservesRecoveryFailureClassification(t *testing.T) {
	receiveOperation, err := NewReceiveOperationID(bytes16(20))
	if err != nil {
		t.Fatal(err)
	}
	outputSession, err := NewOutputSessionID(bytes16(40))
	if err != nil {
		t.Fatal(err)
	}
	receiveIntent, err := NewReceiveIntentDigest(bytes.Repeat([]byte{0x5a}, ReceiveIntentDigestBytes))
	if err != nil {
		t.Fatal(err)
	}
	failure, err := NewFailure(FailureCheckpointStateIO)
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
	if event.Command() != CommandGet || event.Level() != LevelDebug || event.Operation() != spec.Operation {
		t.Fatalf("event envelope = command %d level %d operation %d", event.Command(), event.Level(), event.Operation())
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
	if got, ok := event.Failure(); !ok || got.Code() != FailureCheckpointStateIO {
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
		{"stage without failure", func(value *FilesystemOutputSpec) { value.Failure = Failure{} }},
		{"reconciliation outside recovery", func(value *FilesystemOutputSpec) { value.FailureStage = FilesystemFailureDestinationBinding }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := spec
			test.mutate(&invalid)
			if _, err := NewFilesystemOutputObserved(invalid); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid filesystem event error = %v", err)
			}
		})
	}
}

func TestDiagnosticEventsEnforceBoundedImmutableContracts(t *testing.T) {
	session, err := NewProtocolSessionID(bytes16(60))
	if err != nil {
		t.Fatal(err)
	}
	lane, err := NewLaneIdentity(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	settlementSpec := LaneSettlementSpec{
		Session: session, Route: LaneRouteDirect, Lane: lane,
		DeliveredBlocks: 5, DeliveredBytes: 8192, FailedBlockAttempts: 2,
		ReassignedBlocks: 1, Incomplete: true,
	}
	settlement, err := NewLaneSettlementObserved(settlementSpec)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Command() != CommandGet || settlement.Level() != LevelDebug ||
		settlement.ProtocolSessionID() != session || settlement.Route() != LaneRouteDirect || settlement.Lane() != lane ||
		settlement.DeliveredBlocks() != 5 || settlement.DeliveredBytes() != 8192 ||
		settlement.FailedBlockAttempts() != 2 || settlement.ReassignedBlocks() != 1 || !settlement.Incomplete() {
		t.Fatalf("lane settlement = %+v", settlement)
	}
	for _, invalid := range []LaneSettlementSpec{
		{Route: LaneRouteDirect, Lane: lane},
		{Session: session, Route: LaneRouteDirect},
		{Session: session, Route: LaneRoute(255), Lane: lane},
	} {
		if _, err := NewLaneSettlementObserved(invalid); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid lane settlement error = %v", err)
		}
	}

	loss, err := NewObserverLossObserved(ObserverLossSpec{
		Command: CommandShare, Category: ObserverLossRelayLifecycle,
		Reason: ObserverLossTraceQueue, Count: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if loss.Command() != CommandShare || loss.Level() != LevelDebug ||
		loss.Category() != ObserverLossRelayLifecycle || loss.Reason() != ObserverLossTraceQueue || loss.Count() != 4 {
		t.Fatalf("observer loss = %+v", loss)
	}
	for _, invalid := range []ObserverLossSpec{
		{Category: ObserverLossRelayLifecycle, Reason: ObserverLossTraceQueue, Count: 1},
		{Command: CommandGet, Category: 255, Reason: ObserverLossTraceQueue, Count: 1},
		{Command: CommandGet, Category: ObserverLossRelayLifecycle, Reason: 255, Count: 1},
		{Command: CommandGet, Category: ObserverLossRelayLifecycle, Reason: ObserverLossTraceQueue},
	} {
		if _, err := NewObserverLossObserved(invalid); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid observer loss error = %v", err)
		}
	}

	operation, err := NewProtocolOperationID(bytes16(80))
	if err != nil {
		t.Fatal(err)
	}
	benign := []ReceiverBenignComponent{ReceiverBenignContextCanceled}
	causes := []ReceiverCauseClass{ReceiverCauseDeadlineExceeded}
	transitions := []PeerTeardownTransition{PeerTeardownShutdownInitiated, PeerTeardownShutdownReturned}
	terminationSpec := ReceiverTerminationSpec{
		Operation: operation, HasOperation: true, LocalGeneration: 7,
		TransitionAuthority: ReceiverTerminalLocal, Disposition: ReceiverFallbackAllowed,
		TransitionProvenance:  ReceiverProvenanceLocalExplicitStop,
		ConsequenceProvenance: ReceiverProvenanceLocalContextEnded,
		LocalStopReason:       ReceiverLocalStopOutputAdmission, DiagnosticsTruncated: true,
		BenignComponents: benign, RetainedCauseClasses: causes, TeardownTransitions: transitions,
		PeerShutdownFailed: true, ChannelDrainFailed: true,
	}
	termination, err := NewReceiverTerminationObserved(terminationSpec)
	if err != nil {
		t.Fatal(err)
	}
	benign[0], causes[0], transitions[0] = ReceiverBenignRemoteFinalOperationMissing, ReceiverCauseUnknown, PeerTeardownChannelDrainStarted
	if got, ok := termination.OperationID(); !ok || got != operation || termination.Command() != CommandGet || termination.Level() != LevelDebug ||
		termination.LocalGeneration() != 7 || termination.TransitionAuthority() != ReceiverTerminalLocal ||
		termination.Disposition() != ReceiverFallbackAllowed || termination.TransitionProvenance() != ReceiverProvenanceLocalExplicitStop ||
		termination.ConsequenceProvenance() != ReceiverProvenanceLocalContextEnded ||
		termination.LocalStopReason() != ReceiverLocalStopOutputAdmission || !termination.DiagnosticsTruncated() ||
		!termination.PeerShutdownFailed() || !termination.ChannelDrainFailed() {
		t.Fatalf("receiver termination = %+v operation=%+v,%t", termination, got, ok)
	}
	gotBenign := termination.BenignComponents()
	gotCauses := termination.RetainedCauseClasses()
	gotTransitions := termination.TeardownTransitions()
	if gotBenign[0] != ReceiverBenignContextCanceled || gotCauses[0] != ReceiverCauseDeadlineExceeded ||
		gotTransitions[0] != PeerTeardownShutdownInitiated {
		t.Fatalf("receiver termination aliased input slices: %v %v %v", gotBenign, gotCauses, gotTransitions)
	}
	gotBenign[0] = ReceiverBenignRemoteFinalOperationMissing
	if termination.BenignComponents()[0] != ReceiverBenignContextCanceled {
		t.Fatal("receiver termination exposed mutable diagnostic storage")
	}

	baseTermination := ReceiverTerminationSpec{
		LocalGeneration: 1, TransitionAuthority: ReceiverTerminalLocal,
		Disposition: ReceiverFallbackAllowed, TransitionProvenance: ReceiverProvenanceLocalExplicitStop,
		ConsequenceProvenance: ReceiverProvenanceLocalExplicitStop, LocalStopReason: ReceiverLocalStopCaller,
	}
	invalidTermination := []struct {
		name   string
		mutate func(*ReceiverTerminationSpec)
	}{
		{"missing operation", func(value *ReceiverTerminationSpec) { value.HasOperation = true }},
		{"hidden operation", func(value *ReceiverTerminationSpec) { value.Operation = operation }},
		{"unknown benign component", func(value *ReceiverTerminationSpec) { value.BenignComponents = []ReceiverBenignComponent{255} }},
		{"unknown retained cause", func(value *ReceiverTerminationSpec) { value.RetainedCauseClasses = []ReceiverCauseClass{255} }},
		{"unknown teardown transition", func(value *ReceiverTerminationSpec) { value.TeardownTransitions = []PeerTeardownTransition{255} }},
	}
	for _, test := range invalidTermination {
		t.Run(test.name, func(t *testing.T) {
			invalid := baseTermination
			test.mutate(&invalid)
			if _, err := NewReceiverTerminationObserved(invalid); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid receiver termination error = %v", err)
			}
		})
	}
}
