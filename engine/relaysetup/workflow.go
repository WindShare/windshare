package relaysetup

import (
	"context"
	"errors"
	"io"
)

// workflow executes against one dependency set. Service owns admission and
// cancellation; a Run scopes transaction observation to its own workflow.
type workflow struct {
	dependencies Dependencies
}

func (run *workflow) emit(progress func(Progress), event Progress) {
	run.dependencies.Log.Info("relay_setup", "stage", event.Stage, "operation_id", event.OperationID,
		"enrollment_id", event.EnrollmentID, "retry_after", event.RetryAfter)
	if progress != nil {
		progress(event)
	}
}
func (run *workflow) execute(ctx context.Context, request Request, progress func(Progress)) (Result, error) {
	if request.Operation != AddProfile && request.Operation != PrivateOnboarding {
		return Result{}, errors.New("unknown relay setup operation")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	saved, err := run.dependencies.Store.Load()
	if err != nil {
		return Result{}, err
	}
	if saved.Pending != nil {
		if !sameRelay(saved.Pending.RelayURL, request.URL) || saved.Pending.Operation != request.Operation {
			return Result{}, ErrPendingSetup
		}
		return run.continueEnrollment(ctx, saved, progress, request.NoBrowser)
	}
	discovery, err := run.discover(ctx, request.URL)
	if err != nil {
		return Result{}, err
	}
	if request.Operation == PrivateOnboarding && discovery.Access != "restricted" {
		return Result{}, errors.New("private onboarding requires a restricted relay")
	}
	if discovery.Access == "open" {
		return run.install(ctx, saved.SettingsRevision(), discovery.RelayURL, "", "", discovery.FrontendURL, request.Operation)
	}
	if request.DeviceName == "" {
		return Result{}, errors.New("device name is required")
	}
	secret := make([]byte, claimSecretBytes)
	if _, err := io.ReadFull(run.dependencies.Random, secret); err != nil {
		return Result{}, err
	}
	operation := make([]byte, operationIDBytes)
	if _, err := io.ReadFull(run.dependencies.Random, operation); err != nil {
		return Result{}, err
	}
	saved.Pending = &PendingSetup{
		SettingsRevision: saved.SettingsRevision(), OperationID: encode(operation),
		RelayURL: discovery.RelayURL, ConsoleURL: discovery.ConsoleURL, FrontURL: discovery.FrontendURL,
		DeviceName: request.DeviceName, ClaimSecret: encode(secret), Operation: request.Operation, phase: requesting{}}

	// Persist the claimant before the first request; a lost response can recover by digest.
	pending := saved.Pending
	saved, err = run.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		if current.Pending != nil {
			return ErrPendingSetup
		}
		if current.SettingsRevision() != pending.SettingsRevision {
			return errSettingsChangedBeforePending
		}
		// Detect capacity and selection failures before asking anyone to approve.
		if _, err := installation(current.Config, pending.RelayURL, "", pending.FrontURL, request.Operation); err != nil {
			return err
		}
		current.Pending = pending
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return run.continueEnrollment(ctx, saved, progress, request.NoBrowser)
}
