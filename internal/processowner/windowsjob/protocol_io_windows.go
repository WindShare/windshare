//go:build windows

package windowsjob

import (
	"errors"
	"io"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

type launcherEventResult struct {
	event launcherEvent
	err   error
}

func readLauncherEvent(reader io.Reader) (launcherEvent, error) {
	event, err := ownerprotocol.ReadFrame[launcherEvent](reader)
	if err != nil {
		return launcherEvent{}, err
	}
	if event.SchemaVersion != launcherEventSchema {
		return launcherEvent{}, errors.New("launcher event schema is unsupported")
	}
	switch event.Type {
	case launcherEventRootStarted:
		if event.PID == 0 || event.ProcessHandle == 0 || event.SpawnFailure != nil {
			return launcherEvent{}, errors.New("root-started launcher event is inconsistent")
		}
	case launcherEventSpawnFailed:
		if event.PID != 0 || event.ProcessHandle != 0 || event.InputHandle != 0 ||
			event.SpawnFailure == nil || *event.SpawnFailure == "" {
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
