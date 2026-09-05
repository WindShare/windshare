//go:build windows

package windowsjob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/sys/windows"
)

const (
	windowsSupervisorStartupTimeout  = 30 * time.Second
	windowsSupervisorCompletionSlack = time.Second
)

func TestRunSupervisesNaturalExitAndDeadline(t *testing.T) {
	t.Run("natural", func(t *testing.T) {
		statuses, err := runWindowsSupervisor(t, "natural", 5*time.Second, 200*time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(statuses) != 2 || statuses[0].State != processowner.StatusStarted ||
			statuses[1].Result == nil || statuses[1].Result.Reason != processowner.ReasonNatural ||
			statuses[1].Result.ExitCode == nil || *statuses[1].Result.ExitCode != 0 {
			t.Fatalf("natural statuses = %s", windowsStatusesDiagnostic(statuses))
		}
	})

	t.Run("deadline", func(t *testing.T) {
		statuses, err := runWindowsSupervisor(t, "deadline", 250*time.Millisecond, 100*time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(statuses) != 2 || statuses[1].Result == nil ||
			statuses[1].Result.Reason != processowner.ReasonDeadline ||
			statuses[1].Result.ExitCode == nil || *statuses[1].Result.ExitCode == 0 ||
			statuses[1].Result.Error != "" || statuses[1].Result.CleanupError != "" {
			t.Fatalf("deadline statuses = %s", windowsStatusesDiagnostic(statuses))
		}
	})
}

func windowsStatusesDiagnostic(statuses []processowner.Status) string {
	encoded, err := json.Marshal(statuses)
	if err != nil {
		return fmt.Sprintf("encode supervisor statuses: %v", err)
	}
	return string(encoded)
}

func TestRunReportsSpawnFailure(t *testing.T) {
	config := windowsSupervisorConfig(t, "natural", time.Second, 100*time.Millisecond)
	config.Executable = filepath.Join(t.TempDir(), "missing.exe")
	statuses, err := collectWindowsSupervisor(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Result == nil ||
		statuses[0].Result.Reason != processowner.ReasonSpawnFailed || statuses[0].Result.Error == "" {
		t.Fatalf("spawn failure statuses = %s", windowsStatusesDiagnostic(statuses))
	}
}

func TestReadControlMapsCommandsAndEOF(t *testing.T) {
	tests := []struct {
		name   string
		input  []byte
		reason string
		failed bool
	}{
		{name: "interrupt", input: []byte{processowner.ControlInterrupt}, reason: processowner.ReasonInterrupt},
		{name: "stop", input: []byte{processowner.ControlStop}, reason: processowner.ReasonStop},
		{name: "parent lost", reason: processowner.ReasonParentLost},
		{name: "invalid", input: []byte{'x'}, reason: processowner.ReasonStop, failed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := make(chan trigger, 1)
			readControl(bytes.NewReader(test.input), result)
			got := <-result
			if got.reason != test.reason || (got.err != nil) != test.failed {
				t.Fatalf("trigger = %+v", got)
			}
		})
	}
}

func TestAwaitInitialOutcomeObservesExitedRootBeforeQueuedSignals(t *testing.T) {
	controls := make(chan trigger, 1)
	controls <- trigger{reason: processowner.ReasonStop, err: errors.New("late control")}
	deadline := time.Unix(1, 0)
	waitCalls := 0
	decision := awaitInitialOutcome(
		rootProcess{},
		controls,
		deadline,
		func(rootProcess, time.Duration) (rootResult, bool) {
			waitCalls++
			return rootResult{exitCode: 17}, true
		},
		func() time.Time { return deadline },
	)
	if waitCalls != 1 || !decision.rootSettled || decision.reason != processowner.ReasonNatural ||
		decision.outcome.exitCode != 17 {
		t.Fatalf("initial decision = %+v after %d root observations", decision, waitCalls)
	}
	if len(controls) != 1 {
		t.Fatal("late control displaced an already observable root exit")
	}
}

func TestAwaitInitialOutcomeUsesQueuedControlBeforeDeadline(t *testing.T) {
	controlErr := errors.New("control failed")
	controls := make(chan trigger, 1)
	controls <- trigger{reason: processowner.ReasonStop, err: controlErr}
	deadline := time.Unix(1, 0)
	decision := awaitInitialOutcome(
		rootProcess{},
		controls,
		deadline,
		func(rootProcess, time.Duration) (rootResult, bool) { return rootResult{}, false },
		func() time.Time { return deadline },
	)
	if decision.rootSettled || decision.reason != processowner.ReasonStop ||
		!errors.Is(decision.controlErr, controlErr) {
		t.Fatalf("initial decision = %+v", decision)
	}
}

func TestWaitMillisecondsPreservesPollingBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		wait time.Duration
		want uint32
	}{
		{name: "expired", wait: -time.Millisecond, want: 0},
		{name: "sub-millisecond", wait: time.Nanosecond, want: 1},
		{name: "poll", wait: jobPollInterval, want: uint32(jobPollInterval.Milliseconds())},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := waitMilliseconds(test.wait); got != test.want {
				t.Fatalf("wait milliseconds = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRunRejectsIncompleteEndpointsAndInvalidConfig(t *testing.T) {
	if err := Run(processowner.Config{}, nil, nil, nil); err == nil {
		t.Fatal("incomplete endpoints were accepted")
	}
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
	defer func() {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
	}()
	if err := Run(processowner.Config{}, statusWriter, controlReader, eventWriter); err == nil {
		t.Fatal("invalid config was accepted")
	}
}

func runWindowsSupervisor(
	t *testing.T,
	mode string,
	deadline time.Duration,
	grace time.Duration,
	control []byte,
) ([]processowner.Status, error) {
	t.Helper()
	return collectWindowsSupervisor(windowsSupervisorConfig(t, mode, deadline, grace), control)
}

func windowsSupervisorConfig(
	t *testing.T,
	mode string,
	deadline time.Duration,
	grace time.Duration,
) processowner.Config {
	t.Helper()
	executable, err := filepath.Abs(os.Getenv("ComSpec"))
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"/d", "/s", "/c", "exit 0"}
	if mode == "deadline" {
		arguments = []string{"/d", "/s", "/c", "ping -t 127.0.0.1 >nul"}
	}
	return processowner.Config{
		Executable: executable, Arguments: arguments,
		WorkingDirectory: filepath.Dir(executable), Environment: cleanTestEnvironment(os.Environ()),
		DeadlineMilliseconds: deadline.Milliseconds(), TerminationGraceMilliseconds: grace.Milliseconds(),
	}
}

func cleanTestEnvironment(source []string) []string {
	values := make(map[string]string, len(source))
	for _, entry := range source {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" {
			continue
		}
		values[strings.ToUpper(name)] = entry
	}
	result := make([]string, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	return result
}

func collectWindowsSupervisor(
	config processowner.Config,
	control []byte,
) ([]processowner.Status, error) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, statusReader.Close(), statusWriter.Close())
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(
			err,
			statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
		)
	}
	defer closeTestFiles(statusReader, controlWriter, eventReader)
	done := make(chan error, 1)
	go func() {
		runErr := Run(config, statusWriter, controlReader, eventWriter)
		done <- errors.Join(runErr, closeTestFiles(statusWriter, controlReader, eventWriter))
	}()
	if control != nil {
		if _, err := controlWriter.Write(control); err != nil {
			return nil, err
		}
	}
	collected := make(chan statusCollection, 1)
	go func() { collected <- collectStatuses(statusReader) }()
	// Run starts the configured deadline only after the OS process is contained
	// and ready. The harness therefore composes an independent startup budget
	// instead of charging process creation and host scanning to child execution.
	maximum := windowsSupervisorStartupTimeout + time.Duration(
		config.DeadlineMilliseconds+config.TerminationGraceMilliseconds,
	)*time.Millisecond + windowsSupervisorCompletionSlack
	timer := time.NewTimer(maximum)
	defer timer.Stop()
	var collection statusCollection
	var runErr error
	for collected != nil || done != nil {
		select {
		case collection = <-collected:
			collected = nil
		case runErr = <-done:
			done = nil
		case <-timer.C:
			_ = controlWriter.Close()
			pending := make([]string, 0, 2)
			if collected != nil {
				pending = append(pending, "terminal_status")
			}
			if done != nil {
				pending = append(pending, "run_return")
			}
			return nil, fmt.Errorf(
				"windows supervisor did not settle within its startup and lifecycle bounds: pending=%s observed_statuses=%d",
				strings.Join(pending, ","),
				len(collection.statuses),
			)
		}
	}
	if runErr != nil || collection.err != nil {
		return nil, errors.Join(runErr, collection.err)
	}
	if _, err := io.Copy(io.Discard, eventReader); err != nil {
		return nil, err
	}
	return collection.statuses, nil
}

func closeTestFiles(files ...*os.File) error {
	var result error
	for _, file := range files {
		if file == nil {
			continue
		}
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}

type statusCollection struct {
	statuses []processowner.Status
	err      error
}

func collectStatuses(reader io.Reader) statusCollection {
	decoder := processowner.NewStatusDecoder(reader)
	statuses := make([]processowner.Status, 0, 2)
	for len(statuses) < 2 {
		status, err := decoder.Next()
		if err != nil {
			return statusCollection{err: err}
		}
		statuses = append(statuses, status)
		if status.State == processowner.StatusFinished {
			break
		}
	}
	return statusCollection{statuses: statuses}
}

func TestWindowsEnvironmentReplacesEventTransportAndSorts(t *testing.T) {
	block := windowsEnvironment([]string{
		"z=last",
		testtrace.EventFDEnvironment + "=3",
		"A=first",
	}, windows.Handle(42))
	encoded := string(runes(block))
	if strings.Contains(encoded, testtrace.EventFDEnvironment+"=3") ||
		!strings.Contains(encoded, testtrace.EventHandleEnvironment+"=42") {
		t.Fatalf("environment block = %q", encoded)
	}
	if strings.Index(encoded, "A=first") > strings.Index(encoded, "z=last") {
		t.Fatalf("environment block is not sorted: %q", encoded)
	}
}

func TestUniqueHandlesPreservesFirstOccurrence(t *testing.T) {
	got := uniqueHandles([]windows.Handle{1, 2, 1, 3})
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("unique handles = %v", got)
	}
}

func runes(value []uint16) []rune {
	result := make([]rune, 0, len(value))
	for _, character := range value {
		result = append(result, rune(character))
	}
	return result
}
