package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
)

func TestEngineLifecycleProjectionPreservesTaskIdentityAndIndependentFailure(t *testing.T) {
	for _, state := range []engine.TaskState{engine.TaskRunning, engine.TaskStopping, engine.TaskFinished} {
		for _, reason := range []engine.StopReason{engine.NoStop, engine.Cancelled, engine.ShareStopped, engine.ApplicationClosed} {
			settlement := engine.TaskSettlement{}
			if state == engine.TaskFinished {
				cleanupErr := errors.New("cleanup failed")
				settlement = engine.TaskSettlement{Outcome: engine.OutcomeFailed, FailureClass: engine.FailureLocal, Err: cleanupErr, CleanupError: cleanupErr}
			}
			envelope := engine.Observation{TaskID: "application:42", At: time.Unix(10, 0), Event: engine.LifecycleObservation{
				State: state, StopReason: reason, Settlement: settlement,
			}}
			spec, relevant, err := projectEngineTask(clievent.CommandGet, envelope)
			if err != nil || !relevant || spec.Task.String() != string(envelope.TaskID) || spec.At != envelope.At ||
				spec.CleanupFailure.Valid() != (state == engine.TaskFinished) || spec.Failure.Valid() != (state == engine.TaskFinished) {
				t.Fatalf("projection = %+v, %v, %v", spec, relevant, err)
			}
			if _, err := clievent.NewEngineTaskObserved(spec); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, event := range []engine.LifecycleObservation{{State: 255}, {State: engine.TaskRunning, StopReason: 255}} {
		if _, _, err := projectEngineTask(clievent.CommandGet, engine.Observation{TaskID: "application", Event: event}); err == nil {
			t.Fatal("invalid lifecycle accepted")
		}
	}
	if _, _, err := projectEngineTask(clievent.CommandGet, engine.Observation{TaskID: "bad id", Event: engine.LifecycleObservation{State: engine.TaskRunning}}); err == nil {
		t.Fatal("invalid task identity accepted")
	}
}

func TestReceiveParameterFailuresReachGenericTaskTrace(t *testing.T) {
	application, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for _, request := range []engine.ReceiveRequest{
		{},
		{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: engine.ConnectivityPolicy(255)},
	} {
		current, err := application.StartReceive(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		result, err := current.Wait(context.Background())
		if err != nil || result.Err == nil || result.FailureClass != engine.FailureUsage {
			t.Fatalf("receive = %+v, %v", result, err)
		}
		finished := 0
		for observation := range current.Observations() {
			lifecycle, ok := observation.Event.(engine.LifecycleObservation)
			if !ok || lifecycle.State != engine.TaskFinished {
				continue
			}
			finished++
			spec, relevant, err := projectEngineTask(clievent.CommandGet, observation)
			if err != nil || !relevant || spec.Outcome != clievent.EngineOutcomeFailed || spec.FailureClass != clievent.EngineFailureUsage || !spec.Failure.Valid() {
				t.Fatalf("generic trace lost parameter failure: %+v, %v", spec, err)
			}
			if request.Capability.Suite == 0 && spec.Failure.Code() != clievent.FailureInvalidInput {
				t.Fatalf("coded failure was reclassified: %+v", spec)
			}
			if _, err := clievent.NewEngineTaskObserved(spec); err != nil {
				t.Fatal(err)
			}
		}
		if finished != 1 {
			t.Fatalf("finished observations = %d", finished)
		}
	}
}

func TestEngineOutcomeProjectionPreservesBusinessSettlement(t *testing.T) {
	cause := errors.New("task reason")
	for _, test := range []struct {
		settlement engine.TaskSettlement
		outcome    clievent.EngineTaskOutcome
		class      clievent.EngineFailureClass
	}{
		{engine.TaskSettlement{Outcome: engine.OutcomeSuccess}, clievent.EngineOutcomeSuccess, clievent.EngineFailureNone},
		{engine.TaskSettlement{Outcome: engine.OutcomePartial, FailureClass: engine.FailureSourceDrift, Err: cause}, clievent.EngineOutcomePartial, clievent.EngineFailureSourceDrift},
		{engine.TaskSettlement{Outcome: engine.OutcomePaused, FailureClass: engine.FailureNetwork, Err: cause}, clievent.EngineOutcomePaused, clievent.EngineFailureNetwork},
		{engine.TaskSettlement{Outcome: engine.OutcomeCancelled}, clievent.EngineOutcomeCancelled, clievent.EngineFailureNone},
		{engine.TaskSettlement{Outcome: engine.OutcomeStopped}, clievent.EngineOutcomeStopped, clievent.EngineFailureNone},
		{engine.TaskSettlement{Outcome: engine.OutcomeFailed, FailureClass: engine.FailureLocal, Err: cause, CleanupError: cause}, clievent.EngineOutcomeFailed, clievent.EngineFailureLocal},
	} {
		spec, relevant, err := projectEngineTask(clievent.CommandGet, engine.Observation{TaskID: "application:1", Event: engine.LifecycleObservation{State: engine.TaskFinished, Settlement: test.settlement}})
		if err != nil || !relevant || spec.Outcome != test.outcome || spec.FailureClass != test.class {
			t.Fatalf("projection = %+v, %v", spec, err)
		}
		if _, err := clievent.NewEngineTaskObserved(spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, lifecycle := range []engine.LifecycleObservation{
		{State: engine.TaskFinished},
		{State: engine.TaskFinished, Settlement: engine.TaskSettlement{Outcome: engine.OutcomeFailed}},
		{State: engine.TaskRunning, Settlement: engine.TaskSettlement{Outcome: engine.OutcomeSuccess}},
	} {
		if _, _, err := projectEngineTask(clievent.CommandGet, engine.Observation{TaskID: "application:1", Event: lifecycle}); err == nil {
			t.Fatalf("accepted inconsistent lifecycle: %+v", lifecycle)
		}
	}
}

func TestSourceAcquisitionTraceKeepsScopeAndSkipsUnrelatedObservations(t *testing.T) {
	for _, milestone := range []engine.ShareMilestone{engine.ShareMilestoneSourceAcquiring, engine.ShareMilestoneSourceAcquired, engine.ShareMilestoneSourceAcquisitionFailed} {
		envelope := engine.Observation{TaskID: "application:1", At: time.Unix(2, 0), Event: engine.ShareObservation{
			Milestone: milestone, ShareInstance: catalog.ShareInstance{7}, Failure: errors.New("source unavailable"),
		}}
		spec, relevant, err := projectEngineTask(clievent.CommandShare, envelope)
		if err != nil || !relevant || !spec.ShareInstance.Valid() || !spec.Failure.Valid() {
			t.Fatalf("projection = %+v, %v", spec, err)
		}
		if _, err := clievent.NewEngineTaskObserved(spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []engine.Event{nil, engine.ShareObservation{Milestone: engine.ShareMilestoneActivated}, engine.ReceiveWarning{}} {
		if _, relevant, err := projectEngineTask(clievent.CommandGet, engine.Observation{Event: event}); err != nil || relevant {
			t.Fatalf("unrelated projection: %v, %v", relevant, err)
		}
	}
	observeEngineTask(nil, clievent.CommandGet, engine.Observation{})
}

func TestSessionGenerationAndAdmissionProjectionKeepOperationOwnership(t *testing.T) {
	operation, job := receivecontract.OperationID{1}, transfer.TransferJobID{2}
	current, previous := protocolsession.ProtocolSessionID{3}, protocolsession.ProtocolSessionID{4}
	for _, event := range []engine.Event{
		engine.ReceiveGenerationChanged{Operation: operation, Job: job, Current: current, Previous: previous},
		engine.ReceiveAdmissionObserved{Operation: operation, Job: job, Session: current, Trigger: "relay_only_policy", TerminalOwner: "none"},
	} {
		spec, relevant, err := projectEngineTask(clievent.CommandGet, engine.Observation{TaskID: "operation:1", Event: event})
		if err != nil || !relevant || !spec.Operation.Valid() || !spec.Job.Valid() || !spec.Session.Valid() {
			t.Fatalf("operation attribution = %+v, %v", spec, err)
		}
		if _, err := clievent.NewEngineTaskObserved(spec); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShareAdapterExportsTaskAndSourceAcquisitionFailureMilestones(t *testing.T) {
	trace := newRecordingUserTrace()
	app := &App{
		Stdout: io.Discard, Stderr: io.Discard,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return trace, nil
		},
	}
	missing := filepath.Join(t.TempDir(), "missing")
	code := app.Run(context.Background(), []string{"share", missing, "--trace", filepath.Join(t.TempDir(), "trace.ndjson")})
	if code != ExitUsage {
		t.Fatalf("missing source exit = %d", code)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	stages := make(map[clievent.EngineTaskStage]clievent.EngineTaskSpec)
	var taskID clievent.EngineTaskID
	for _, event := range trace.events {
		if typed, ok := event.(clievent.EngineTaskObserved); ok {
			facts := typed.Facts()
			if !taskID.Valid() {
				taskID = facts.Task
			}
			if facts.Task != taskID {
				t.Fatal("adapter changed task identity across milestones")
			}
			stages[facts.Stage] = facts
		}
	}
	for _, stage := range []clievent.EngineTaskStage{clievent.EngineTaskStarted, clievent.EngineSourceAcquiring, clievent.EngineSourceFailed, clievent.EngineTaskSettled} {
		if _, ok := stages[stage]; !ok {
			t.Fatalf("missing engine ownership milestone %v", stage)
		}
	}
	if stages[clievent.EngineSourceAcquiring].ShareInstance != stages[clievent.EngineSourceFailed].ShareInstance ||
		!stages[clievent.EngineSourceFailed].Failure.Valid() || !stages[clievent.EngineTaskSettled].Failure.Valid() {
		t.Fatal("source failure lost its scope or final error")
	}
}
