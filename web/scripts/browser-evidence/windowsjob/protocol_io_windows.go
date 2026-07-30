//go:build windows

package main

import (
	"errors"
	"io"
)

type launcherEventResult struct {
	event launcherEvent
	err   error
}

func readLauncherEvent(reader io.Reader) (launcherEvent, error) {
	event, err := readCanonicalFrame[launcherEvent](reader, "launcher event")
	if err != nil {
		return launcherEvent{}, err
	}
	if event.SchemaVersion != protocolSchemaVersion {
		return launcherEvent{}, errors.New("launcher event schema is unsupported")
	}
	switch event.Type {
	case launcherEventRootStarted:
		if event.PID == 0 || event.ProcessHandle == 0 || event.SpawnFailure != nil {
			return launcherEvent{}, errors.New("root-started launcher event is inconsistent")
		}
	case launcherEventSpawnFailed:
		if event.PID != 0 || event.ProcessHandle != 0 || event.SpawnFailure == nil || *event.SpawnFailure == "" {
			return launcherEvent{}, errors.New("spawn-failed launcher event is inconsistent")
		}
		if boundedDiagnostic(errors.New(*event.SpawnFailure)) != *event.SpawnFailure {
			return launcherEvent{}, errors.New("spawn-failed launcher diagnostic is not canonical bounded text")
		}
	default:
		return launcherEvent{}, errors.New("launcher event type is unsupported")
	}
	return event, nil
}

func readControlRequests(reader io.Reader, start startRequest) <-chan controlResult {
	results := make(chan controlResult, 1)
	go func() {
		request, err := readCanonicalFrame[terminateRequest](reader, "control request")
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("parent control channel disconnected")
			}
			results <- controlResult{err: err}
			return
		}
		if err := validateTerminateRequest(request, start); err != nil {
			results <- controlResult{err: err}
			return
		}
		results <- controlResult{request: request}
	}()
	return results
}
