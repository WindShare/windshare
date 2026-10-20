package relaysetup

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func (service *Service) awaitCredential(ctx context.Context, pending *PendingSetup, progress func(Progress)) (Snapshot, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if service.dependencies.Now().UnixMilli() >= pending.enrollment().ExpiresAt {
			return Snapshot{}, ErrExpired
		}
		current, err := service.currentPending(pending)
		if err != nil {
			return Snapshot{}, err
		}
		if current.SettingsRevision() != pending.SettingsRevision {
			service.dependencies.Log.Info("relay_setup_settings_changed", "operation_id", pending.OperationID,
				"enrollment_id", pending.enrollment().ID)
			return Snapshot{}, ErrSettingsChanged
		}
		var credential credentialResponse
		err = service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments/claim"), claimant(pending), &credential)
		if err == nil {
			if credential.ID == "" || credential.AccessKey == "" {
				return Snapshot{}, errors.New("empty enrollment credential")
			}
			// Retain cancellation authority even if settings changed during the
			// HTTP call; only installation may publish the issued credential.
			return service.checkpoint(ctx, pending, func(current *PendingSetup) setupPhase {
				return installing{Enrollment: current.enrollment(), Credential: credentialValue(credential)}
			})
		}
		var failure *apiFailure
		if !errors.As(err, &failure) || !failure.temporary() {
			return Snapshot{}, err
		}
		delay := max(failure.delay(), time.Duration(pending.enrollment().IntervalMS)*time.Millisecond)
		service.emit(progress, Progress{Stage: failure.ErrorCode, OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID, RetryAfter: delay})
		if err = service.dependencies.Wait(ctx, delay); err != nil {
			return Snapshot{}, err
		}
	}
}
