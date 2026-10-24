package eventproject

import (
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestEngineLifecycleProjectionPreservesTaskIdentityAndIndependentFailure(t *testing.T) {
	for _, state := range []task.State{task.Running, task.Stopping, task.Finished} {
		for _, reason := range []task.StopReason{task.NoStop, task.Cancelled, task.ShareStopped, task.ApplicationClosed} {
			settlement := task.Settlement{}
			if state == task.Finished {
				cleanupErr := errors.New("cleanup failed")
				settlement = task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: cleanupErr, CleanupError: cleanupErr}
			}
			settlement.Failure, _ = ClassifyError(settlement.Err)
			settlement.CleanupFailure, _ = ClassifyError(settlement.CleanupError)
			envelope := task.Observation{TaskID: "application:42", At: time.Unix(10, 0), Event: task.Lifecycle{
				State: state, StopReason: reason, Settlement: settlement,
			}}
			spec, relevant, err := projectTaskSpec(appobs.WorkflowReceive, envelope)
			if err != nil || !relevant || spec.Task.String() != string(envelope.TaskID) || spec.At != envelope.At ||
				spec.CleanupFailure.Valid() != (state == task.Finished) || spec.Failure.Valid() != (state == task.Finished) {
				t.Fatalf("projection = %+v, %v, %v", spec, relevant, err)
			}
			if _, err := appobs.NewEngineTaskObserved(spec); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, event := range []task.Lifecycle{{State: 255}, {State: task.Running, StopReason: 255}} {
		if _, _, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "application", Event: event}); err == nil {
			t.Fatal("invalid lifecycle accepted")
		}
	}
	if _, _, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "bad id", Event: task.Lifecycle{State: task.Running}}); err == nil {
		t.Fatal("invalid task identity accepted")
	}
}

func TestEngineOutcomeProjectionPreservesBusinessSettlement(t *testing.T) {
	cause := errors.New("task reason")
	for _, test := range []struct {
		settlement task.Settlement
		outcome    appobs.EngineTaskOutcome
		class      appobs.EngineFailureClass
	}{
		{task.Settlement{Outcome: task.OutcomeSuccess}, appobs.EngineOutcomeSuccess, appobs.EngineFailureNone},
		{task.Settlement{Outcome: task.OutcomePartial, FailureClass: task.FailureSourceDrift, Err: cause}, appobs.EngineOutcomePartial, appobs.EngineFailureSourceDrift},
		{task.Settlement{Outcome: task.OutcomePaused, FailureClass: task.FailureNetwork, Err: cause}, appobs.EngineOutcomePaused, appobs.EngineFailureNetwork},
		{task.Settlement{Outcome: task.OutcomeCancelled}, appobs.EngineOutcomeCancelled, appobs.EngineFailureNone},
		{task.Settlement{Outcome: task.OutcomeStopped}, appobs.EngineOutcomeStopped, appobs.EngineFailureNone},
		{task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: cause, CleanupError: cause}, appobs.EngineOutcomeFailed, appobs.EngineFailureLocal},
	} {
		test.settlement.Failure, _ = ClassifyError(test.settlement.Err)
		test.settlement.CleanupFailure, _ = ClassifyError(test.settlement.CleanupError)
		spec, relevant, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "application:1", Event: task.Lifecycle{State: task.Finished, Settlement: test.settlement}})
		if err != nil || !relevant || spec.Outcome != test.outcome || spec.FailureClass != test.class {
			t.Fatalf("projection = %+v, %v", spec, err)
		}
		if _, err := appobs.NewEngineTaskObserved(spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, lifecycle := range []task.Lifecycle{
		{State: task.Finished},
		{State: task.Finished, Settlement: task.Settlement{Outcome: task.OutcomeFailed}},
		{State: task.Running, Settlement: task.Settlement{Outcome: task.OutcomeSuccess}},
	} {
		if _, _, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "application:1", Event: lifecycle}); err == nil {
			t.Fatalf("accepted inconsistent lifecycle: %+v", lifecycle)
		}
	}
}

func TestSourceAcquisitionTraceKeepsScopeAndSkipsUnrelatedObservations(t *testing.T) {
	for _, milestone := range []share.Milestone{share.SourceAcquiring, share.SourceAcquired, share.SourceAcquisitionFailed} {
		envelope := task.Observation{TaskID: "application:1", At: time.Unix(2, 0), Event: share.Observation{
			Milestone: milestone, ShareInstance: catalog.ShareInstance{7}, Failure: errors.New("source unavailable"),
		}}
		spec, relevant, err := projectTaskSpec(appobs.WorkflowShare, envelope)
		if err != nil || !relevant || !spec.ShareInstance.Valid() || !spec.Failure.Valid() {
			t.Fatalf("projection = %+v, %v", spec, err)
		}
		if _, err := appobs.NewEngineTaskObserved(spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []task.Event{nil, share.Observation{Milestone: share.ShareReady}, receive.Warning{}} {
		if _, relevant, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{Event: event}); err != nil || relevant {
			t.Fatalf("unrelated projection: %v, %v", relevant, err)
		}
	}
}

func TestSessionGenerationAndAdmissionProjectionKeepOperationOwnership(t *testing.T) {
	operation, job := receivecontract.OperationID{1}, transfer.TransferJobID{2}
	current, previous := protocolsession.ProtocolSessionID{3}, protocolsession.ProtocolSessionID{4}
	for _, event := range []task.Event{
		receive.GenerationChanged{Operation: operation, Job: job, Current: current, Previous: previous},
		receive.AdmissionObserved{Operation: operation, Job: job, Session: current, Trigger: "relay_only_policy", TerminalOwner: "none"},
	} {
		spec, relevant, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "operation:1", Event: event})
		if err != nil || !relevant || !spec.Operation.Valid() || !spec.Job.Valid() || !spec.Session.Valid() {
			t.Fatalf("operation attribution = %+v, %v", spec, err)
		}
		if _, err := appobs.NewEngineTaskObserved(spec); err != nil {
			t.Fatal(err)
		}
	}
}
