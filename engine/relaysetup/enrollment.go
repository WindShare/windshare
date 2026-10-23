package relaysetup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
)

type enrollmentResponse struct {
	ID              string `json:"enrollment_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_uri_complete"`
	ExpiresAt       int64  `json:"expires_at"`
	IntervalMS      int64  `json:"interval_ms"`
}
type credentialResponse struct {
	AccessKey string `json:"access_key"`
	ID        string `json:"credential_id"`
}

func encode(bytes []byte) string { return base64.RawURLEncoding.EncodeToString(bytes) }
func claimant(pending *PendingSetup) map[string]string {
	return map[string]string{"enrollment_id": pending.enrollment().ID, "claim_secret": pending.ClaimSecret}
}
func (service *Service) continueEnrollment(ctx context.Context, saved Snapshot, progress func(Progress), noBrowser bool) (Result, error) {
	if saved.Pending.phase.stage() == (cancelling{}).stage() {
		if err := service.finishCancellation(ctx, saved.Pending); err != nil {
			return Result{}, err
		}
		return Result{}, ErrSetupSuperseded
	}
	return service.enroll(ctx, saved, progress, noBrowser)
}

func (service *Service) enroll(ctx context.Context, saved Snapshot, progress func(Progress), noBrowser bool) (result Result, resultErr error) {
	pending := saved.Pending
	defer func() {
		if errors.Is(resultErr, ErrExpired) {
			service.emit(progress, Progress{Stage: "expired", OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID})
			resultErr = errors.Join(resultErr, service.clearPending(ctx, pending))
		}
	}()
	if pending.enrollment().ID == "" {
		var err error
		saved, err = service.requestEnrollment(ctx, saved)
		if err != nil {
			return Result{}, err
		}
		pending = saved.Pending
	}
	if pending.phase.stage() == (installing{}).stage() {
		return service.confirm(ctx, saved, progress)
	}
	if service.dependencies.Now().UnixMilli() >= pending.enrollment().ExpiresAt {
		return Result{}, ErrExpired
	}
	event := Progress{Stage: "approval_required", OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID,
		VerificationURL: pending.enrollment().VerificationURL, UserCode: pending.enrollment().UserCode}
	service.emit(progress, event)
	if !noBrowser && service.dependencies.OpenBrowser != nil {
		if err := service.dependencies.OpenBrowser(ctx, pending.enrollment().VerificationURL); err != nil {
			event.Stage = "browser_unavailable"
			service.emit(progress, event)
		}
	}
	claimed, err := service.awaitCredential(ctx, pending, progress)
	if err != nil {
		return Result{}, err
	}
	pending = claimed.Pending
	return service.confirm(ctx, claimed, progress)
}
func (service *Service) confirm(ctx context.Context, saved Snapshot, progress func(Progress)) (Result, error) {
	pending := saved.Pending
	var response struct {
		Services []struct {
			turn.Capability
			RelayURL string `json:"relay_url"`
		} `json:"managed_turn"`
	}
	requestedAt := service.dependencies.Now()
	for {
		current, err := service.dependencies.Store.Load()
		if err != nil {
			return Result{}, err
		}
		if _, err := pendingInstallation(current, pending); err != nil {
			service.dependencies.Log.Info("relay_installation_preflight_failed", "operation_id", pending.OperationID,
				"enrollment_id", pending.enrollment().ID, "reason", err)
			return Result{}, err
		}
		err = service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments/installed"), claimant(pending), &response)
		if err == nil {
			break
		}
		var failure *apiFailure
		if !errors.As(err, &failure) || !failure.temporary() {
			return Result{}, err
		}
		service.emit(progress, Progress{Stage: failure.ErrorCode, OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID, RetryAfter: failure.delay()})
		if err := service.dependencies.Wait(ctx, failure.delay()); err != nil {
			return Result{}, err
		}
	}
	committed, err := service.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		next, err := pendingInstallation(*current, pending)
		if err == nil {
			current.Config, current.Pending = next, nil
		}
		return err
	})
	if err != nil {
		service.dependencies.Log.Info("relay_installation_commit_failed", "operation_id", pending.OperationID,
			"enrollment_id", pending.enrollment().ID, "credential_id", pending.credential().ID, "reason", err)
		return Result{}, err
	}
	if service.dependencies.Cache != nil && len(response.Services) == 1 {
		discovered := response.Services[0]
		if sameRelay(discovered.RelayURL, pending.RelayURL) && discovered.Validate() == nil {
			candidate := relayprofile.Candidate{PublicURL: pending.RelayURL, AccessKey: pending.credential().AccessKey}
			if err := service.dependencies.Cache.Save(ctx, candidate, discovered.Capability, requestedAt); err != nil {
				service.dependencies.Log.Debug("relay_setup_cache_failed", "operation_id", pending.OperationID, "reason", err)
			}
		}
	}
	service.emit(progress, Progress{Stage: "installed", OperationID: pending.OperationID, EnrollmentID: pending.enrollment().ID})
	service.dependencies.Log.Info("relay_configuration_installed", "operation_id", pending.OperationID,
		"enrollment_id", pending.enrollment().ID, "credential_id", pending.credential().ID, "relay_url", pending.RelayURL)
	return installationResult(committed.Config, pending.RelayURL, pending.credential().ID), nil
}

func (service *Service) requestEnrollment(ctx context.Context, saved Snapshot) (Snapshot, error) {
	pending := saved.Pending
	secret, err := base64.RawURLEncoding.DecodeString(pending.ClaimSecret)
	if err != nil || len(secret) != claimSecretBytes {
		return Snapshot{}, relayconfig.ErrConfig
	}
	digest := sha256.Sum256(secret)
	var response enrollmentResponse
	if err := service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments"),
		map[string]string{"device_name": pending.DeviceName, "claim_digest": encode(digest[:])}, &response); err != nil {
		return Snapshot{}, err
	}
	parsed, parseErr := url.Parse(response.VerificationURL)
	console, _ := url.Parse(pending.ConsoleURL)
	if parseErr != nil || parsed.User != nil || parsed.Scheme != console.Scheme || parsed.Host != console.Host ||
		response.ID == "" || response.UserCode == "" || response.IntervalMS < minimumPollIntervalMS || response.IntervalMS > maximumPollIntervalMS ||
		response.ExpiresAt <= service.dependencies.Now().UnixMilli() {
		return Snapshot{}, errors.New("invalid enrollment response")
	}
	return service.checkpoint(ctx, pending, func(*PendingSetup) setupPhase {
		return awaiting{Enrollment: enrollment(response)}
	})
}

func credentialValue(value credentialResponse) credential {
	return credential(value)
}
