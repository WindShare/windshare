package relaysetup

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/windshare/windshare/engine/relayprofile"
)

// workflow executes against one dependency set. Service owns admission and
// cancellation; a Run scopes transaction observation to its own workflow.
type workflow struct {
	dependencies Dependencies
}

func (run *workflow) emit(progress func(Progress), event Progress) {
	run.dependencies.Log.Info("relay_setup", "stage", event.Stage, "operation_id", event.OperationID,
		"enrollment_id", event.EnrollmentID, "service_login", event.ServiceLogin != nil, "retry_after", event.RetryAfter)
	if progress != nil {
		progress(event)
	}
}
func (run *workflow) execute(ctx context.Context, request Request, progress func(Progress)) (Result, error) {
	if request.Operation != AddProfile && request.Operation != AuthorizeDevice {
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
		if !saved.Pending.matches(request) {
			return Result{}, ErrPendingSetup
		}
		return run.continueEnrollment(ctx, saved, progress)
	}
	if request.AccessKey != "" {
		profile := saved.CredentialProfile(request.URL, request.AccessKey)
		// Manual credentials also work with offline relays. Discovery is public
		// and bounded so an unavailable console cannot prevent installation.
		const capabilityTimeout = 3 * time.Second
		probe, cancel := context.WithTimeout(ctx, capabilityTimeout)
		capability, err := run.discover(probe, request.URL)
		cancel()
		if err == nil {
			profile.ManagedTurn = capability.ManagedTurn
			profile.ConsoleURL = capability.ConsoleURL
			profile.OfferedFrontend = capability.OfferedFrontend
		}
		return run.install(ctx, saved.SettingsRevision(), profile)
	}
	discovery, err := run.discover(ctx, request.URL)
	if err != nil {
		return Result{}, err
	}
	candidate := relayprofile.Candidate{PublicURL: discovery.RelayURL, ConsoleURL: discovery.ConsoleURL, ManagedTurn: discovery.ManagedTurn, OfferedFrontend: discovery.OfferedFrontend}
	if discovery.Access == "open" && request.Operation == AddProfile {
		return run.install(ctx, saved.SettingsRevision(), candidate)
	}
	if discovery.ConsoleURL == "" {
		if discovery.Access == nativeKeyAccess {
			return Result{}, ErrLocalCredentialRequired
		}
		return Result{}, errors.New("this relay does not support device pairing")
	}
	if err := ValidateDeviceName(request.DeviceName); err != nil {
		return Result{}, err
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
		RelayURL: discovery.RelayURL, ConsoleURL: discovery.ConsoleURL, ManagedTurn: discovery.ManagedTurn, OfferedFrontend: discovery.OfferedFrontend,
		AccountLoginPage: discovery.AccountLoginPage, DeviceName: request.DeviceName, ClaimSecret: encode(secret), Operation: request.Operation, phase: requesting{}}

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
		if _, err := current.MergeRelay(pending.profile("")); err != nil {
			return err
		}
		current.Pending = pending
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return run.continueEnrollment(ctx, saved, progress)
}
