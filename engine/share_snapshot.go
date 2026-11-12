package engine

import (
	"context"

	"github.com/windshare/windshare/engine/internal/eventproject"
	appobs "github.com/windshare/windshare/engine/observation"
)

type ShareStartupState uint8

const (
	SharePreparing ShareStartupState = iota
	ShareReadyReached
	ShareStartupFailed
)

// ShareSnapshot separates retained startup from lifecycle and current relay
// admission. Existing P2P transfers do not depend on relay availability.
type ShareSnapshot struct {
	State             TaskState
	Startup           ShareStartupState
	Ready             ShareReady
	StartupError      error
	RelayAvailability *appobs.RelayAvailability
	// Result is present only after Done closes: all task work has joined.
	Result *TaskResult[ShareResult]
}

func (current *ShareTask) Snapshot() ShareSnapshot {
	snapshot := ShareSnapshot{State: current.State()}
	ready, err, settled := current.controller.Startup()
	if settled {
		snapshot.Startup = ShareStartupFailed
		snapshot.StartupError = err
		if err == nil {
			origin, projectionErr := eventproject.RelayOrigin(ready.RelayEndpoint)
			snapshot.StartupError = projectionErr
			if projectionErr == nil {
				snapshot.Startup = ShareReadyReached
				snapshot.Ready = ShareReady{Capability: ready.Capability, SelectedRootSummary: ready.SelectedRootSummary, RelayOrigin: origin}
			}
		}
	}
	if value, ok := current.controller.Availability(); ok {
		available, err := appobs.NewRelayAvailability(value.Available, value.Total, value.Terminal, value.EverReady)
		if err == nil {
			snapshot.RelayAvailability = &available
		}
	}
	select {
	case <-current.Done():
		result, _ := current.Wait(context.Background())
		snapshot.Result = &result
		snapshot.State = TaskFinished
	default:
	}
	return snapshot
}
