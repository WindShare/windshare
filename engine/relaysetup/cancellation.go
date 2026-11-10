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
		if interrupted {
			return nil
		}
		// Reporting success here would hide a mistyped URL or an already
		// finished setup behind "cancelled".
		return ErrNoPendingSetup
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
