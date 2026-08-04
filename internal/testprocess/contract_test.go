package testprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

func TestEnvironmentOverridesAndRejectsInvalidEntries(t *testing.T) {
	t.Setenv(testtrace.EventFDEnvironment, "99")
	t.Setenv("WINDSHARE_TESTPROCESS_OVERRIDE", "old")
	environment, err := InheritEnvironment(map[string]string{
		"windshare_testprocess_override": "new",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.ToUpper(strings.Join(environment, "\n"))
	if strings.Contains(joined, strings.ToUpper(testtrace.EventFDEnvironment)+"=") ||
		!strings.Contains(joined, "WINDSHARE_TESTPROCESS_OVERRIDE=NEW") {
		t.Fatalf("environment = %v", environment)
	}
	for _, overrides := range []map[string]string{
		{"": "value"}, {"A=B": "value"}, {"A": "bad\x00value"},
	} {
		if _, err := InheritEnvironment(overrides); err == nil {
			t.Fatalf("invalid overrides %v were accepted", overrides)
		}
	}
}

func TestBoundedOutputRetainsTailAndBoundsReadiness(t *testing.T) {
	output := newBoundedOutput()
	prefix := bytes.Repeat([]byte{'x'}, MaximumCapturedOutputBytes)
	if _, err := output.Write(append(prefix, []byte("ready")...)); err != nil {
		t.Fatal(err)
	}
	snapshot := output.snapshot()
	if !snapshot.Truncated || len(snapshot.Bytes) != MaximumCapturedOutputBytes ||
		!strings.HasSuffix(snapshot.String(), "ready") {
		t.Fatalf("snapshot length=%d truncated=%v suffix=%q", len(snapshot.Bytes), snapshot.Truncated, snapshot.String()[len(snapshot.Bytes)-5:])
	}
	done := make(chan struct{})
	match, err := output.waitFor(t.Context(), done, regexp.MustCompile(`(ready)$`))
	if err != nil || len(match) != 2 || match[1] != "ready" {
		t.Fatalf("readiness = %v, %v", match, err)
	}
	close(done)
	if _, err := output.waitFor(t.Context(), done, regexp.MustCompile(`missing`)); !errors.Is(err, errReadinessBeforeExit) {
		t.Fatalf("post-exit readiness error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newBoundedOutput().waitFor(ctx, make(chan struct{}), regexp.MustCompile(`missing`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled readiness error = %v", err)
	}
}

func TestEventReaderValidatesSchemaAndReportsStreamErrors(t *testing.T) {
	event := testrun.Event{
		SchemaVersion: testrun.EventSchemaVersion,
		Identity: testrun.Identity{
			RunID: "run-1", OperationID: "operation-1", Scenario: "event-reader",
		},
		Component: "component", Milestone: "ready", Outcome: string(testrun.OutcomeSucceeded),
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeEvent(encoded)
	if err != nil || decoded.Identity != event.Identity {
		t.Fatalf("decoded event = %+v, %v", decoded, err)
	}
	for _, invalid := range [][]byte{
		append(bytes.Clone(encoded), []byte(" {}")...),
		[]byte(`{"schema_version":"unknown"}`),
		[]byte(`{"unknown":true}`),
	} {
		if _, err := decodeEvent(invalid); err == nil {
			t.Fatalf("invalid event %q was accepted", invalid)
		}
	}
	stream := append(append(bytes.Clone(encoded), '\n'), []byte("invalid\n")...)
	reader := newEventReader(io.NopCloser(bytes.NewReader(stream)))
	if got, err := reader.Next(t.Context()); err != nil || got.Milestone != event.Milestone {
		t.Fatalf("stream event = %+v, %v", got, err)
	}
	<-reader.Done()
	if _, err := reader.Next(t.Context()); err == nil {
		t.Fatal("invalid stream tail was not reported")
	}
}

func TestEventReaderSettlementClosesAStalledSource(t *testing.T) {
	source, inheritedWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inheritedWriter.Close()
	reader := newEventReader(source)
	if err := waitForEventReader(reader, source, 20*time.Millisecond); err == nil {
		t.Fatal("stalled event source did not report its lifecycle bound")
	}
	select {
	case <-reader.Done():
	case <-time.After(time.Second):
		t.Fatal("event reader remained blocked after its source was closed")
	}
}

func TestOwnerAndSpecValidationFailuresAreExplicit(t *testing.T) {
	if _, err := NewOwner("relative"); err == nil {
		t.Fatal("relative helper path was accepted")
	}
	directory := t.TempDir()
	if _, err := NewOwner(directory); err == nil {
		t.Fatal("helper directory was accepted")
	}
	if _, err := NewOwner(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing helper was accepted")
	}
	if err := (*Owner)(nil).SelfCheck(t.Context()); err == nil {
		t.Fatal("nil owner self-check was accepted")
	}
	if _, err := (*Owner)(nil).Start(t.Context(), Spec{}); err == nil {
		t.Fatal("nil owner start was accepted")
	}
	closed := &Owner{closed: true}
	if _, err := closed.Start(t.Context(), Spec{}); err == nil {
		t.Fatal("closed owner start was accepted")
	}
	active := &Owner{active: 1}
	if err := active.Close(); err == nil {
		t.Fatal("active owner was closed")
	}
	owner := &Owner{}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	invalid := Spec{Identity: testrun.Identity{RunID: "bad id"}}
	if _, err := configForSpec(invalid); err == nil {
		t.Fatal("invalid identity was accepted")
	}
	if output := boundedBuildOutput(bytes.Repeat([]byte{'x'}, maximumBuildOutput+10)); len(output) != maximumBuildOutput {
		t.Fatalf("bounded build output length = %d", len(output))
	}
	t.Setenv("WINDSHARE_GO_EXECUTABLE", "go-custom")
	if got := goExecutable(); got != "go-custom" {
		t.Fatalf("Go executable = %q", got)
	}
}

func TestProcessDiagnosticsAndResultRequirements(t *testing.T) {
	done := make(chan struct{})
	process := &Process{
		identity: "operation-test", stdout: newBoundedOutput(), stderr: newBoundedOutput(),
		control: nopWriteCloser{Writer: io.Discard}, done: done,
	}
	if diagnostic := process.Diagnostic(); !strings.Contains(diagnostic, "operation-test") {
		t.Fatalf("diagnostic = %q", diagnostic)
	}
	if _, err := process.WaitForOutput(t.Context(), Stdout, nil); err == nil {
		t.Fatal("nil readiness pattern was accepted")
	}
	if _, err := process.WaitForOutput(t.Context(), "log", regexp.MustCompile("ready")); err == nil {
		t.Fatal("unknown readiness stream was accepted")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := process.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait error = %v", err)
	}

	zero := int64(0)
	nonzero := int64(3)
	if err := RequireSuccess(Result{ExitCode: &zero, Reason: processowner.ReasonNatural}, nil); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		result Result
		err    error
	}{
		{result: Result{ExitCode: &nonzero, Reason: processowner.ReasonNatural}},
		{result: Result{ExitCode: &zero, Reason: processowner.ReasonNatural}, err: errors.New("lifecycle")},
		{result: Result{ExitCode: &zero, Reason: processowner.ReasonNatural, Error: "target"}},
		{result: Result{ExitCode: &zero, Reason: processowner.ReasonNatural, CleanupError: "cleanup"}},
		{result: Result{Reason: processowner.ReasonNatural}},
	} {
		if err := RequireSuccess(test.result, test.err); err == nil {
			t.Fatalf("failure result %+v, %v was accepted", test.result, test.err)
		}
	}
}

func TestPlatformCommandCloseIsIdempotent(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := &platformCommand{
		status: statusReader, control: controlWriter, events: eventReader,
		child: []*os.File{statusWriter, controlReader, eventWriter},
	}
	if err := command.closeAll(); err != nil {
		t.Fatal(err)
	}
	if err := command.closeAll(); err != nil {
		t.Fatal(err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
