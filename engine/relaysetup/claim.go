package relaysetup

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func (run *workflow) awaitCredential(ctx context.Context, pending *PendingSetup, progress func(Progress)) (Snapshot, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if run.dependencies.Now().UnixMilli() >= pending.enrollment().ExpiresAt {
			return Snapshot{}, ErrExpired
		}
		current, err := run.currentPending(pending)
		if err != nil {
			return Snapshot{}, err
		}
		if current.SettingsRevision() != pending.SettingsRevision {
			run.dependencies.Log.Info("relay_setup_settings_changed", "operation_id", pending.OperationID,
				"enrollment_id", pending.enrollment().ID)
			return Snapshot{}, errSettingsChangedWhilePending
		}
		var credential credentialResponse
		err = run.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments/claim"), claimant(pending), &credential)
		if err == nil {
			if credential.ID == "" || credential.AccessKey == "" {
				return Snapshot{}, errors.New("empty enrollment credential")
			}
			// Retain cancellation authority even if settings changed during the
			// HTTP call; only installation may publish the issued credential.
			return run.checkpoint(ctx, pending, func(current *PendingSetup) setupPhase {
				return installing{Enrollment: current.enrollment(), Credential: credentialValue(credential)}
			})
		}
		var failure *apiFailure
		if !errors.As(err, &failure) || !failure.temporary() {
			return Snapshot{}, err
		}
		delay := max(failure.delay(), time.Duration(pending.enrollment().IntervalMS)*time.Millisecond)
		run.emit(progress, Progress{Stage: failure.ErrorCode, OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID, RetryAfter: delay})
		if err = run.dependencies.Wait(ctx, delay); err != nil {
			return Snapshot{}, err
		}
	}
}
