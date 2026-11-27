package desktop

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/engine/relaysetup"
)

const setupCancellationTimeout = 20 * time.Second

func (a *Application) guestSetup(id string) error {
	if id == "" || a.setup.ID != id || a.setup.State != "running" || !a.setup.GuestAvailable {
		return errors.New("no guest approval is pending")
	}
	a.setup.guestRequested = true
	a.setup.clearApproval()
	a.interruptSetup()
	return nil
}

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
	// A terminal setup has already settled; treat late cancellations as idempotent.
	if a.setup.State == "finished" || a.setup.State == "cancelled" {
		return nil
	}
	// Cancelling relay setup aborts only the setup workflow so a blocked share
	// keeps its selection and can retry with another relay or preference.
	// Cancelling the share itself cascades to setup via Application.cancel.
	a.cancelSetupLocked()
	return nil
}

type setupCleanup struct {
	relayURL string
	pending  bool
}

func setupCleanupFor(relayURL string, relaySaved bool, recovery relaysetup.RecoveryAction) setupCleanup {
	// A finished or unacquired enrollment does not belong to this intent. Do not
	// cancel a different workflow that another process may have started since.
	pending := !relaySaved && relayURL != "" &&
		recovery != relaysetup.RecoveryRestart &&
		recovery != relaysetup.RecoveryBlocked
	return setupCleanup{relayURL: relayURL, pending: pending}
}

// The application fences admission synchronously, then joins Run before asking
// the setup owner to revoke its unfinished enrollment. Committed settings survive.
func (a *Application) cancelSetupLocked() {
	a.setup.guestRequested = false
	switch a.setup.State {
	case "running":
		a.setup.State = "cancelling"
		a.setup.clearApproval()
		a.interruptSetup()
	case "failed", "cancel_failed":
		cleanup := setupCleanupFor(a.setup.RelayURL, a.setup.RelaySaved, a.setup.Recovery)
		a.setup.State, a.setup.Recovery, a.setup.Error = "cancelling", "", nil
		a.work.Go(func() { a.finishSetupCancellation(cleanup) })
	}
}

func (a *Application) finishSetupCancellation(cleanup setupCleanup) {
	ctx, cancel := context.WithTimeout(context.Background(), setupCancellationTimeout)
	var err error
	if cleanup.pending {
		err = a.deps.cancelSetup(ctx, cleanup.relayURL)
	}
	cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setup.clearApproval()
	a.setup.Recovery, a.setup.Error = "", nil
	if err == nil {
		a.setup.State = "cancelled"
	} else {
		a.setup.State = "cancel_failed"
		a.setup.Recovery = relaysetup.RecoveryRetryCancel
		a.setup.Error = failure("setup_cancel_failed", err)
	}
	a.logSetupResult("host.setup_cancelled")
	a.snapshotLocked()
}
