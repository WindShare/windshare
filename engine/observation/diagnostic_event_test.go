package observation

import (
	"errors"
	"testing"
)

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
	if settlement.Workflow() != WorkflowReceive || settlement.Level() != LevelDebug ||
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
		Workflow: WorkflowShare, Category: ObserverLossRelayLifecycle,
		Reason: ObserverLossTraceQueue, Count: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if loss.Workflow() != WorkflowShare || loss.Level() != LevelDebug ||
		loss.Category() != ObserverLossRelayLifecycle || loss.Reason() != ObserverLossTraceQueue || loss.Count() != 4 {
		t.Fatalf("observer loss = %+v", loss)
	}
	for _, invalid := range []ObserverLossSpec{
		{Category: ObserverLossRelayLifecycle, Reason: ObserverLossTraceQueue, Count: 1},
		{Workflow: WorkflowReceive, Category: 255, Reason: ObserverLossTraceQueue, Count: 1},
		{Workflow: WorkflowReceive, Category: ObserverLossRelayLifecycle, Reason: 255, Count: 1},
		{Workflow: WorkflowReceive, Category: ObserverLossRelayLifecycle, Reason: ObserverLossTraceQueue},
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
	if got, ok := termination.OperationID(); !ok || got != operation || termination.Workflow() != WorkflowReceive || termination.Level() != LevelDebug ||
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
		{"unknown benign component", func(value *ReceiverTerminationSpec) {
			value.BenignComponents = []ReceiverBenignComponent{255}
		}},
		{"unknown retained cause", func(value *ReceiverTerminationSpec) {
			value.RetainedCauseClasses = []ReceiverCauseClass{255}
		}},
		{"unknown teardown transition", func(value *ReceiverTerminationSpec) {
			value.TeardownTransitions = []PeerTeardownTransition{255}
		}},
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
