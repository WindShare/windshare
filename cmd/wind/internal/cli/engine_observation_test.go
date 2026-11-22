package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
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
			lifecycle, ok := observation.Event.(engine.TaskLifecycle)
			if !ok || lifecycle.Stage != "task_settled" {
				continue
			}
			finished++
			if lifecycle.Outcome != "failed" || lifecycle.FailureClass != "usage" || lifecycle.Failure == nil || !lifecycle.Failure.Valid() {
				t.Fatalf("generic trace lost parameter failure: %+v", lifecycle)
			}
			if request.Capability.Suite == 0 && lifecycle.Failure.Code != engine.FailureInvalidInput {
				t.Fatalf("coded failure was reclassified: %+v", lifecycle)
			}
			if !lifecycle.DiagnosticRecord().Valid() {
				t.Fatal("invalid task lifecycle diagnostic record")
			}
		}
		if finished != 1 {
			t.Fatalf("finished observations = %d", finished)
		}
	}
}

func TestShareAdapterExportsTaskAndSourceAcquisitionFailureMilestones(t *testing.T) {
	trace := newFakeUserTrace(diaglog.RunStatus{})
	var stderr bytes.Buffer
	app := &App{
		Stdout: io.Discard, Stderr: &stderr,
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return trace, nil
		},
	}
	missing := filepath.Join(t.TempDir(), "missing")
	code := app.Run(context.Background(), []string{"share", missing, "--front-url", "https://front.example", "--relay", "https://relay.example", "--trace", filepath.Join(t.TempDir(), "trace.ndjson")})
	if code != ExitUsage {
		t.Fatalf("missing source exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "Path: "+missing) {
		t.Fatalf("stderr did not report selected source path: %q", stderr.String())
	}
	type taskPayload struct {
		TaskID  string `json:"engine_task_id"`
		Stage   string `json:"stage"`
		Failure *struct {
			Code string `json:"code"`
		} `json:"failure"`
	}
	stages := make(map[string]taskPayload)
	var taskID string
	sources := make(map[string]diaglog.RunRecord)
	for _, rec := range trace.recorded() {
		switch rec.Event {
		case "engine_task_observed":
			var p taskPayload
			decodeRecordPayload(t, rec.Payload, &p)
			if taskID == "" {
				taskID = p.TaskID
			}
			if p.TaskID != taskID {
				t.Fatal("adapter changed task identity across milestones")
			}
			stages[p.Stage] = p
		case "share_workflow":
			var p struct {
				Stage string `json:"stage"`
			}
			decodeRecordPayload(t, rec.Payload, &p)
			sources[p.Stage] = rec
		}
	}
	for _, stage := range []string{"task_started", "task_settled"} {
		if _, ok := stages[stage]; !ok {
			t.Fatalf("missing engine ownership milestone %q", stage)
		}
	}
	settled := stages["task_settled"]
	if settled.Failure == nil || settled.Failure.Code != "source_missing" {
		t.Fatalf("source failure lost its reason: %+v", settled)
	}
	acquiring, failed := sources["source_acquiring"], sources["source_acquisition_failed"]
	if acquiring.Scope == nil || failed.Scope == nil || failed.Scope.TaskID != taskID || *acquiring.Scope != *failed.Scope {
		t.Fatalf("source failure lost scope: %+v / %+v", acquiring, failed)
	}
	var acqCorr, failCorr struct {
		ShareInstanceID string `json:"share_instance_id"`
	}
	decodeRecordPayload(t, acquiring.Correlation, &acqCorr)
	decodeRecordPayload(t, failed.Correlation, &failCorr)
	var failPayload struct {
		Failure string `json:"failure"`
	}
	decodeRecordPayload(t, failed.Payload, &failPayload)
	if acqCorr.ShareInstanceID == "" || acqCorr.ShareInstanceID != failCorr.ShareInstanceID || !strings.Contains(failPayload.Failure, strconv.Quote(missing)) {
		t.Fatalf("source failure lost details: corr=%+v/%+v payload=%+v", acqCorr, failCorr, failPayload)
	}
}

// Two selected files named alike (a/README.md, b/README.md) cannot both be
// top-level catalog entries. The share must stop as a usage error that preserves
// both selections for diagnostics and presentation.
func TestShareReportsSelectedPathsThatCollideByName(t *testing.T) {
	trace := newFakeUserTrace(diaglog.RunStatus{})
	var stderr bytes.Buffer
	app := &App{
		Stdout: io.Discard, Stderr: &stderr,
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return trace, nil
		},
	}
	base := t.TempDir()
	var selected []string
	for _, folder := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(base, folder), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(base, folder, "README.md")
		if err := os.WriteFile(path, []byte(folder), 0o600); err != nil {
			t.Fatal(err)
		}
		selected = append(selected, path)
	}
	args := append([]string{"share"}, selected...)
	args = append(args, "--front-url", "https://front.example", "--relay", "https://relay.example",
		"--trace", filepath.Join(t.TempDir(), "trace.ndjson"))
	if code := app.Run(context.Background(), args); code != ExitUsage {
		t.Fatalf("colliding selection exit = %d", code)
	}
	if !strings.Contains(stderr.String(), selected[0]) || !strings.Contains(stderr.String(), selected[1]) {
		t.Fatalf("stderr did not report both colliding paths: %q", stderr.String())
	}
	for _, rec := range trace.recorded() {
		if rec.Event != "engine_task_observed" {
			continue
		}
		var p struct {
			Stage   string `json:"stage"`
			Failure *struct {
				Code string `json:"code"`
			} `json:"failure"`
		}
		decodeRecordPayload(t, rec.Payload, &p)
		if p.Stage != "task_settled" {
			continue
		}
		if p.Failure == nil || p.Failure.Code != "selection_collision" {
			t.Fatalf("colliding selection lost its reason: %+v", p)
		}
		return
	}
	t.Fatal("share never settled")
}
