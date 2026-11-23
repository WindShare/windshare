package engine

import (
	"context"

	"github.com/windshare/windshare/core/catalog"
)

type ShareStartupState uint8

const (
	SharePreparing ShareStartupState = iota
	ShareAwaitingAuthorization
	ShareReadyReached
	ShareStartupFailed
)

// ShareSourceOmissions retains the cumulative count and bounded examples of
// entries skipped across directories discovered so far during a share.
type ShareSourceOmissions struct {
	Count   uint64
	Samples []catalog.SourceOmission
}

// ShareSnapshot separates retained startup from lifecycle and current relay
// admission. Existing P2P transfers do not depend on relay availability.
type ShareSnapshot struct {
	State             TaskState
	Startup           ShareStartupState
	Ready             ShareReady
	StartupError      error
	Authorization     *ShareAuthorization
	RelayAvailability *RelayAvailability
	SourceOmissions   *ShareSourceOmissions
	// Result is present only after Done closes: all task work has joined.
	Result *TaskResult[ShareResult]
}

func (current *ShareTask) Snapshot() ShareSnapshot {
	select {
	case <-current.Done():
		return current.terminalSnapshot()
	default:
	}
	snapshot := current.controllerSnapshot()
	// Read lifecycle after controller state so cancellation cannot pair a newer
	// readiness result with a lifecycle sampled before the stop.
	snapshot.State = current.State()
	select {
	case <-current.Done():
		return current.terminalSnapshot()
	default:
	}
	return snapshot
}

// terminalSnapshot reads controller state only after Done has closed so every
// field happens-after background scans, shutdown joins, and controller.finish.
func (current *ShareTask) terminalSnapshot() ShareSnapshot {
	snapshot := current.controllerSnapshot()
	result, _ := current.Wait(context.Background())
	snapshot.Result = &result
	snapshot.State = TaskFinished
	if result.Outcome == OutcomeFailed && snapshot.Startup == ShareStartupFailed {
		snapshot.Authorization = shareAuthorization(snapshot.StartupError, current.selection)
	}
	return snapshot
}

func (current *ShareTask) controllerSnapshot() ShareSnapshot {
	var snapshot ShareSnapshot
	status, hasStatus := current.controller.RelayStatus()
	_, err, settled := current.controller.Startup()
	if settled {
		snapshot.Startup = ShareStartupFailed
		snapshot.StartupError = err
		if err == nil {
			ready, projectionErr := current.Ready(context.Background())
			snapshot.StartupError = projectionErr
			if projectionErr == nil {
				snapshot.Startup = ShareReadyReached
				snapshot.Ready = ready
			}
		}
	} else if status.AuthorizationRequired {
		snapshot.Startup = ShareAwaitingAuthorization
		snapshot.Authorization = &ShareAuthorization{Kind: ShareOfficialAuthorization}
	}
	if hasStatus && status.Availability.Total > 0 {
		value := status.Availability
		snapshot.RelayAvailability = &RelayAvailability{
			Available: value.Available,
			Total:     value.Total,
			EverReady: value.EverReady,
			Terminal:  value.Terminal,
		}
	}
	if omissions, hasOmissions := current.controller.SourceOmissions(); hasOmissions {
		snapshot.SourceOmissions = &ShareSourceOmissions{
			Count:   omissions.Count,
			Samples: omissions.Samples,
		}
	}
	return snapshot
}
