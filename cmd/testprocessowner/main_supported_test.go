//go:build linux || windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner"
)

func TestRunPlatformAdoptsEndpointsAndSupervisesTarget(t *testing.T) {
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
		_ = controlWriter.Close()
		_ = eventReader.Close()
	}()
	arguments := []string{
		"--status-" + ownerEndpointSuffix, strconv.FormatUint(uint64(statusWriter.Fd()), 10),
		"--control-" + ownerEndpointSuffix, strconv.FormatUint(uint64(controlReader.Fd()), 10),
		"--event-" + ownerEndpointSuffix, strconv.FormatUint(uint64(eventWriter.Fd()), 10),
	}
	config := commandOwnerConfig(t)
	done := make(chan error, 1)
	go func() { done <- runPlatform(arguments, config) }()
	decoder := processowner.NewStatusDecoder(statusReader)
	for _, want := range []string{processowner.StatusStarted, processowner.StatusFinished} {
		status, err := decoder.Next()
		if err != nil || status.State != want {
			t.Fatalf("status = %+v, %v; want %s", status, err, want)
		}
		if status.Result != nil && (status.Result.ExitCode == nil || *status.Result.ExitCode != 0) {
			t.Fatalf("terminal result = %+v", status.Result)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, eventReader); err != nil {
		t.Fatal(err)
	}
}

func TestInheritedFilesRejectMalformedArguments(t *testing.T) {
	statusOption := "--status-" + ownerEndpointSuffix
	controlOption := "--control-" + ownerEndpointSuffix
	eventOption := "--event-" + ownerEndpointSuffix
	tests := [][]string{
		nil,
		{"--wrong-" + ownerEndpointSuffix, "1", controlOption, "2", eventOption, "3"},
		{statusOption, "bad", controlOption, "2", eventOption, "3"},
		{statusOption, "0", controlOption, "2", eventOption, "3"},
	}
	for _, arguments := range tests {
		if _, err := inheritedFiles(arguments, ownerEndpointSuffix); err == nil {
			t.Fatalf("arguments %v were accepted", arguments)
		}
	}
}

func commandOwnerConfig(t *testing.T) processowner.Config {
	t.Helper()
	executable, arguments := ownedTargetCommand(t)
	return processowner.Config{
		Executable: executable, Arguments: arguments,
		WorkingDirectory: filepath.Dir(executable), Environment: commandOwnerEnvironment(os.Environ()),
		DeadlineMilliseconds:         (5 * time.Second).Milliseconds(),
		TerminationGraceMilliseconds: (time.Second).Milliseconds(),
	}
}

func commandOwnerEnvironment(source []string) []string {
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
