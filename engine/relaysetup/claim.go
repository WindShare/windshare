package relaysetup

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
)

func (service *Service) awaitCredential(ctx context.Context, pending *relayconfig.PendingSetup, progress func(Progress)) (relayconfig.Config, error) {
	for {
		if err := ctx.Err(); err != nil {
			return relayconfig.Config{}, err
		}
		if service.dependencies.Now().UnixMilli() >= pending.ExpiresAt {
			return relayconfig.Config{}, ErrExpired
		}
		current, err := service.currentPending(pending)
		if err != nil {
			return relayconfig.Config{}, err
		}
		if current.SettingsRevision() != pending.SettingsRevision {
			service.dependencies.Log.Info("relay_setup_settings_changed", "operation_id", pending.OperationID,
				"enrollment_id", pending.EnrollmentID)
			return relayconfig.Config{}, ErrSettingsChanged
		}
		var credential credentialResponse
		err = service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments/claim"), claimant(pending), &credential)
		if err == nil {
			if credential.ID == "" || credential.AccessKey == "" {
				return relayconfig.Config{}, errors.New("empty enrollment credential")
			}
			// Retain cancellation authority even if settings changed during the
			// HTTP call; only installation may publish the issued credential.
			return service.checkpoint(ctx, pending, func(current *relayconfig.PendingSetup) {
				current.CredentialID = credential.ID
				current.AccessKey = credential.AccessKey
				current.State = relayconfig.SetupInstalling
			})
		}
		var failure *apiFailure
		if !errors.As(err, &failure) || !failure.temporary() {
			return relayconfig.Config{}, err
		}
		delay := max(failure.delay(), time.Duration(pending.IntervalMS)*time.Millisecond)
		service.emit(progress, Progress{Stage: failure.ErrorCode, OperationID: pending.OperationID, EnrollmentID: pending.EnrollmentID, RetryAfter: delay})
		if err = service.dependencies.Wait(ctx, delay); err != nil {
			return relayconfig.Config{}, err
		}
	}
}
