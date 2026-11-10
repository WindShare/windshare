package relaysetup

import (
	"context"
	"errors"
	"net/http"
)

func (service *Service) Cancel(ctx context.Context, address string) error {
	finish, interrupted, err := service.beginCancellation(ctx, address)
	if err != nil {
		return err
	}
	defer finish()
	if service.dependencies.Store == nil {
		return errors.New("relay configuration storage is unavailable")
	}
	saved, err := service.dependencies.Store.Load()
	if err != nil {
		return err
	}
	if saved.Pending == nil {
		service.dependencies.Log.Info("relay_cancel_without_pending", "address", address, "interruption", interrupted.String())
		switch interrupted {
		case stoppedRunning:
			// The setup stopped before saving a pending record (for example
			// during discovery), so stopping it is the whole cancellation.
			return nil
		case finishedFirst:
			return errFinishedBeforeCancel
		default:
			// Reporting success here would hide a mistyped URL or an already
			// finished setup behind "cancelled".
			return ErrNoPendingSetup
		}
	}
	if !sameRelay(saved.Pending.RelayURL, address) {
		return ErrPendingSetup
	}
	saved, err = service.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		if err := ownsPending(*current, saved.Pending); err != nil {
			return err
		}
		// Commit the cancellation fence before contacting the server so another
		// process cannot install a credential whose revocation is in flight.
		current.Pending = current.Pending.withPhase(cancelling{EnrollmentID: current.Pending.enrollment().ID})
		return nil
	})
	if err != nil {
		return err
	}
	return service.finishCancellation(ctx, saved.Pending)
}

func (service *Service) finishCancellation(ctx context.Context, pending *PendingSetup) error {
	if pending.enrollment().ID != "" {
		var response map[string]any
		err := service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments/cancel"), claimant(pending), &response)
		if err != nil && !errors.Is(err, ErrExpired) {
			return err
		}
	}
	if err := service.clearPending(ctx, pending); err != nil {
		return err
	}
	service.emit(nil, Progress{Stage: "cancelled", OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID})
	return nil
}

// HasPending reports whether a setup for address is persisted and resumable,
// so a caller can describe what an interruption left behind.
func (service *Service) HasPending(address string) (bool, error) {
	if service.dependencies.Store == nil {
		return false, errors.New("relay configuration storage is unavailable")
	}
	saved, err := service.dependencies.Store.Load()
	if err != nil {
		return false, err
	}
	return saved.Pending != nil && sameRelay(saved.Pending.RelayURL, address), nil
}
