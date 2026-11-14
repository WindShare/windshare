package desktop

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/engine/relaysetup"
)

const setupCancellationTimeout = 20 * time.Second

func (a *Application) cancelSetupRequest(id string) error {
	if id == "" {
		return errors.New("setup_id is required")
	}
	if a.setup.ID != id {
		if _, known := a.setupRequests[id]; known {
			return nil
		}
		return errors.New("relay setup not found")
	}
	if a.setup.OperationID != "" {
		return a.cancel(a.setup.OperationID)
	}
	a.cancelSetupLocked()
	return nil
}

// The application fences admission synchronously, then joins Run before asking
// the setup owner to revoke its unfinished enrollment. Committed settings survive.
func (a *Application) cancelSetupLocked() {
	switch a.setup.State {
	case "running":
		a.setup.State = "cancelling"
		a.setup.VerificationURL, a.setup.UserCode = "", ""
		a.interruptSetup()
	case "failed", "cancel_failed":
		a.setup.State, a.setup.Error = "cancelling", nil
		a.work.Go(a.finishSetupCancellation)
	}
}

func (a *Application) finishSetupCancellation() {
	a.mu.Lock()
	address := a.setup.RelayURL
	noPending := a.setup.RelaySaved || a.setup.Recovery == relaysetup.RecoveryRestart || a.setup.Recovery == relaysetup.RecoveryBlocked
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), setupCancellationTimeout)
	var err error
	// A finished or unacquired enrollment does not belong to this intent. Do not
	// cancel a different workflow that another process may have started since.
	if !noPending {
		err = a.deps.cancelSetup(ctx, address)
	}
	cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setup.VerificationURL, a.setup.UserCode = "", ""
	a.setup.Recovery, a.setup.Error = "", nil
	if err == nil {
		a.setup.State = "cancelled"
	} else {
		a.setup.State = "cancel_failed"
		a.setup.Recovery = relaysetup.RecoveryRetryCancel
		a.setup.Error = failure("setup_cancel_failed", err)
	}
	a.deps.log("host.setup_cancelled", a.setup)
	a.snapshotLocked()
}
