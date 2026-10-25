package cli

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

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
			lifecycle, ok := observation.Event.(appobs.EngineTaskObserved)
			if !ok || lifecycle.Facts().Stage != appobs.EngineTaskSettled {
				continue
			}
			finished++
			spec := lifecycle.Facts()
			relevant, err := true, lifecycle.Validate()
			if err != nil || !relevant || spec.Outcome != appobs.EngineOutcomeFailed || spec.FailureClass != appobs.EngineFailureUsage || !spec.Failure.Valid() {
				t.Fatalf("generic trace lost parameter failure: %+v, %v", spec, err)
			}
			if request.Capability.Suite == 0 && spec.Failure.Code() != appobs.FailureInvalidInput {
				t.Fatalf("coded failure was reclassified: %+v", spec)
			}
			if _, err := appobs.NewEngineTaskObserved(spec); err != nil {
				t.Fatal(err)
			}
		}
		if finished != 1 {
			t.Fatalf("finished observations = %d", finished)
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
	code := app.Run(context.Background(), []string{"share", missing, "--front-url", "https://front.example", "--relay", "https://relay.example", "--trace", filepath.Join(t.TempDir(), "trace.ndjson")})
	if code != ExitUsage {
		t.Fatalf("missing source exit = %d", code)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	stages := make(map[appobs.EngineTaskStage]appobs.EngineTaskSpec)
	var taskID appobs.EngineTaskID
	for _, event := range trace.events {
		if typed, ok := event.(appobs.EngineTaskObserved); ok {
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
	for _, stage := range []appobs.EngineTaskStage{appobs.EngineTaskStarted, appobs.EngineTaskSettled} {
		if _, ok := stages[stage]; !ok {
			t.Fatalf("missing engine ownership milestone %v", stage)
		}
	}
	for _, stage := range []appobs.EngineTaskStage{appobs.EngineTaskSettled} {
		failure := stages[stage].Failure
		path, located := failure.SourcePath()
		if failure.Code() != appobs.FailureSourceMissing || !located || path.Text() != missing {
			t.Fatalf("source failure lost its reason or selected path at %v: %+v", stage, failure)
		}
	}
	sources := map[string]diagnostics.Record{}
	for _, event := range trace.events {
		if value, ok := event.(clievent.Diagnostic); ok && value.Record.Name() == "share_workflow" {
			sources[value.Record.Payload().Get("stage").Text()] = value.Record
		}
	}
	acquiring, failed := sources["source_acquiring"], sources["source_acquisition_failed"]
	if !acquiring.Valid() || !failed.Valid() || failed.Scope().TaskID != taskID.String() || acquiring.Scope() != failed.Scope() || acquiring.Correlation().Get("share_instance_id").Text() != failed.Correlation().Get("share_instance_id").Text() || !strings.Contains(failed.Payload().Get("failure").Text(), strconv.Quote(missing)) {
		t.Fatalf("source failure lost scope/details: %v", sources)
	}
}
