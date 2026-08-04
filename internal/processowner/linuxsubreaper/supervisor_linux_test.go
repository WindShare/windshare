//go:build linux

package linuxsubreaper

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner"
)

func TestRunSupervisesNaturalExitAndDeadline(t *testing.T) {
	t.Run("natural", func(t *testing.T) {
		statuses, err := runLinuxSupervisor(t, "natural", 5*time.Second, 200*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(statuses) != 2 || statuses[0].State != processowner.StatusStarted ||
			statuses[1].Result == nil || statuses[1].Result.Reason != processowner.ReasonNatural ||
			statuses[1].Result.ExitCode == nil || *statuses[1].Result.ExitCode != 0 {
			t.Fatalf("natural statuses = %+v", statuses)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		statuses, err := runLinuxSupervisor(t, "deadline", 250*time.Millisecond, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(statuses) != 2 || statuses[1].Result == nil ||
			statuses[1].Result.Reason != processowner.ReasonDeadline ||
			statuses[1].Result.ExitCode == nil || *statuses[1].Result.ExitCode == 0 ||
			statuses[1].Result.CleanupError != "" {
			t.Fatalf("deadline statuses = %+v", statuses)
		}
	})
}

func TestRunReportsSpawnFailure(t *testing.T) {
	config := linuxSupervisorConfig(t, "natural", time.Second, 100*time.Millisecond)
	config.Executable = filepath.Join(t.TempDir(), "missing")
	statuses, err := collectLinuxSupervisor(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Result == nil ||
		statuses[0].Result.Reason != processowner.ReasonSpawnFailed || statuses[0].Result.Error == "" {
		t.Fatalf("spawn failure statuses = %+v", statuses)
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

func runLinuxSupervisor(
	t *testing.T,
	mode string,
	deadline time.Duration,
	grace time.Duration,
) ([]processowner.Status, error) {
	t.Helper()
	return collectLinuxSupervisor(linuxSupervisorConfig(t, mode, deadline, grace))
}

func linuxSupervisorConfig(
	t *testing.T,
	mode string,
	deadline time.Duration,
	grace time.Duration,
) processowner.Config {
	t.Helper()
	executable, err := filepath.Abs("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"-c", "exit 0"}
	if mode == "deadline" {
		arguments = []string{"-c", "trap '' INT; while :; do sleep 3600; done"}
	}
	return processowner.Config{
		Executable: executable, Arguments: arguments,
		WorkingDirectory:     filepath.Dir(executable),
		Environment:          os.Environ(),
		DeadlineMilliseconds: deadline.Milliseconds(), TerminationGraceMilliseconds: grace.Milliseconds(),
	}
}

func collectLinuxSupervisor(config processowner.Config) (_ []processowner.Status, resultErr error) {
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
	defer func() { resultErr = errors.Join(resultErr, closeTestFiles(statusReader, controlWriter, eventReader)) }()
	done := make(chan error, 1)
	go func() {
		runErr := Run(config, statusWriter, controlReader, eventWriter)
		done <- errors.Join(runErr, closeTestFiles(statusWriter, controlReader, eventWriter))
	}()
	collected := make(chan statusCollection, 1)
	go func() { collected <- collectStatuses(statusReader) }()
	maximum := time.Duration(
		config.DeadlineMilliseconds+config.TerminationGraceMilliseconds,
	)*time.Millisecond + time.Second
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
			return nil, errors.New("linux supervisor did not settle within its lifecycle bound")
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

func TestParseParentPIDHandlesSpacesAndParentheses(t *testing.T) {
	parent, err := parseParentPID("123 (a command) name) S 456 1 2 3")
	if err != nil || parent != 456 {
		t.Fatalf("parent = %d, %v", parent, err)
	}
	for _, invalid := range []string{"", "12 no-close S 1", "12 (x) S nope"} {
		if _, err := parseParentPID(invalid); err == nil {
			t.Fatalf("invalid stat %q was accepted", invalid)
		}
	}
}

func TestDescendantInventoryIncludesCurrentChildrenOnly(t *testing.T) {
	descendants, err := descendantPIDs(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(descendants) != 0 {
		t.Fatalf("impossible owner descendants = %v", descendants)
	}
}

func TestReplaceEnvironmentRemovesAndAddsValues(t *testing.T) {
	environment := replaceEnvironment(
		[]string{"A=one", "B=two"},
		map[string]string{"A": "replacement", "B": "", "C": "three"},
	)
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "A=one") || strings.Contains(joined, "B=two") ||
		!strings.Contains(joined, "A=replacement") || !strings.Contains(joined, "C=three") {
		t.Fatalf("environment = %v", environment)
	}
}
