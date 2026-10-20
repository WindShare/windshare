package relaysetup

import (
	"context"
	"errors"
	"net/http"

	"github.com/windshare/windshare/engine/relayconfig"
)

func (service *Service) Cancel(ctx context.Context, address string) error {
	finish, err := service.beginCancellation(ctx, address)
	if err != nil {
		return err
	}
	defer finish()
	if service.dependencies.Store == nil {
		return errors.New("relay configuration storage is unavailable")
	}
	saved, err := service.dependencies.Store.Load()
	if err != nil || saved.Pending == nil {
		return err
	}
	if !sameRelay(saved.Pending.RelayURL, address) {
		return ErrPendingSetup
	}
	saved, err = service.dependencies.Store.Update(ctx, func(current *relayconfig.Config) error {
		if err := ownsPending(*current, saved.Pending); err != nil {
			return err
		}
		// Commit the cancellation fence before contacting the server so another
		// process cannot install a credential whose revocation is in flight.
		current.Pending.State = relayconfig.SetupCancelling
		return nil
	})
	if err != nil {
		return err
	}
	return service.finishCancellation(ctx, saved.Pending)
}

func (service *Service) finishCancellation(ctx context.Context, pending *relayconfig.PendingSetup) error {
	if pending.EnrollmentID != "" {
		var response map[string]any
		err := service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments/cancel"), claimant(pending), &response)
		if err != nil && !errors.Is(err, ErrExpired) {
			return err
		}
	}
	if err := service.clearPending(ctx, pending); err != nil {
		return err
	}
	service.emit(nil, Progress{Stage: "cancelled", OperationID: pending.OperationID, EnrollmentID: pending.EnrollmentID})
	return nil
}
