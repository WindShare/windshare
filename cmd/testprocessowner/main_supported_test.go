//go:build linux || windows

package main

import (
	"errors"
	"fmt"
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
	statusReader, statusWriter := newTestPipe(t)
	controlReader, controlWriter := newTestPipe(t)
	eventReader, eventWriter := newTestPipe(t)
	config := commandOwnerConfig(t)

	producerEndpoints := []struct {
		name string
		file *os.File
	}{
		{name: "status", file: statusWriter},
		{name: "control", file: controlReader},
		{name: "event", file: eventWriter},
	}
	inheritedEndpoints := make([]uintptr, 0, len(producerEndpoints))
	inheritedEndpointsOwned := true
	closeUnadoptedEndpoints := func() error {
		if !inheritedEndpointsOwned {
			return nil
		}
		inheritedEndpointsOwned = false
		return closeInheritedEndpoints(inheritedEndpoints)
	}
	defer func() {
		if err := closeUnadoptedEndpoints(); err != nil {
			t.Errorf("close unadopted process-owner endpoints: %v", err)
		}
	}()
	for _, endpoint := range producerEndpoints {
		duplicate, err := duplicateInheritedEndpoint(endpoint.file)
		if err != nil {
			t.Fatal(errors.Join(
				fmt.Errorf("duplicate process-owner %s endpoint: %w", endpoint.name, err),
				closeUnadoptedEndpoints(),
			))
		}
		inheritedEndpoints = append(inheritedEndpoints, duplicate)
	}
	if err := errors.Join(statusWriter.Close(), controlReader.Close(), eventWriter.Close()); err != nil {
		t.Fatalf("retire original process-owner endpoints: %v", err)
	}
	arguments := []string{
		"--status-" + ownerEndpointSuffix, strconv.FormatUint(uint64(inheritedEndpoints[0]), 10),
		"--control-" + ownerEndpointSuffix, strconv.FormatUint(uint64(inheritedEndpoints[1]), 10),
		"--event-" + ownerEndpointSuffix, strconv.FormatUint(uint64(inheritedEndpoints[2]), 10),
	}
	done := make(chan error, 1)
	// The raw duplicates deliberately have no os.File owner until this handoff;
	// runPlatform's adopted wrappers therefore have sole close authority.
	inheritedEndpointsOwned = false
	go func() { done <- runPlatform(arguments, config) }()
	decoder := processowner.NewStatusDecoder(statusReader)
	var lifecycleErr error
	for _, want := range []string{processowner.StatusStarted, processowner.StatusFinished} {
		status, err := decoder.Next()
		if err != nil {
			lifecycleErr = fmt.Errorf("decode %s process status: %w", want, err)
			break
		}
		if status.State != want {
			lifecycleErr = fmt.Errorf("process status = %+v; want %s", status, want)
			break
		}
		if status.Result != nil && (status.Result.ExitCode == nil || *status.Result.ExitCode != 0) {
			lifecycleErr = fmt.Errorf("terminal result = %+v; want exit code 0", status.Result)
			break
		}
	}
	if err := errors.Join(lifecycleErr, <-done); err != nil {
		t.Fatal(err)
	}
	if err := controlWriter.Close(); err != nil {
		t.Fatalf("close process-owner control endpoint: %v", err)
	}
	if _, err := io.Copy(io.Discard, eventReader); err != nil {
		t.Fatal(err)
	}
}

func newTestPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup retains both wrappers until their explicit close, preventing a
	// finalizer from racing descriptor duplication during setup.
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	return reader, writer
}

func closeInheritedEndpoints(endpoints []uintptr) error {
	var result error
	for _, endpoint := range endpoints {
		file := os.NewFile(endpoint, "unadopted-process-owner-endpoint")
		if file == nil {
			result = errors.Join(result, errors.New("unadopted process-owner endpoint is invalid"))
			continue
		}
		result = errors.Join(result, file.Close())
	}
	return result
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
