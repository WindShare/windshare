package relaysetup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"

	"github.com/windshare/windshare/engine/relayconfig"
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
func claimant(pending *relayconfig.PendingSetup) map[string]string {
	return map[string]string{"enrollment_id": pending.EnrollmentID, "claim_secret": pending.ClaimSecret}
}
func (service *Service) continueEnrollment(ctx context.Context, saved relayconfig.Config, progress func(Progress), noBrowser bool) (Result, error) {
	if saved.Pending.State == relayconfig.SetupCancelling {
		if err := service.finishCancellation(ctx, saved.Pending); err != nil {
			return Result{}, err
		}
		return Result{}, ErrSetupSuperseded
	}
	return service.enroll(ctx, saved, progress, noBrowser)
}

func (service *Service) enroll(ctx context.Context, saved relayconfig.Config, progress func(Progress), noBrowser bool) (result Result, resultErr error) {
	pending := saved.Pending
	defer func() {
		if errors.Is(resultErr, ErrExpired) {
			service.emit(progress, Progress{Stage: "expired", OperationID: pending.OperationID, EnrollmentID: pending.EnrollmentID})
			resultErr = errors.Join(resultErr, service.clearPending(ctx, pending))
		}
	}()
	if pending.EnrollmentID == "" {
		var err error
		saved, err = service.requestEnrollment(ctx, saved)
		if err != nil {
			return Result{}, err
		}
		pending = saved.Pending
	}
	if pending.State == relayconfig.SetupInstalling {
		return service.confirm(ctx, saved, progress)
	}
	if service.dependencies.Now().UnixMilli() >= pending.ExpiresAt {
		return Result{}, ErrExpired
	}
	event := Progress{Stage: "approval_required", OperationID: pending.OperationID, EnrollmentID: pending.EnrollmentID,
		VerificationURL: pending.VerificationURL, UserCode: pending.UserCode}
	service.emit(progress, event)
	if !noBrowser && service.dependencies.OpenBrowser != nil {
		if err := service.dependencies.OpenBrowser(ctx, pending.VerificationURL); err != nil {
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
func (service *Service) confirm(ctx context.Context, saved relayconfig.Config, progress func(Progress)) (Result, error) {
	pending := saved.Pending
	var response map[string]any
	for {
		current, err := service.dependencies.Store.Load()
		if err != nil {
			return Result{}, err
		}
		if _, err := pendingInstallation(current, pending); err != nil {
			service.dependencies.Log.Info("relay_installation_preflight_failed", "operation_id", pending.OperationID,
				"enrollment_id", pending.EnrollmentID, "reason", err)
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
		service.emit(progress, Progress{Stage: failure.ErrorCode, OperationID: pending.OperationID, EnrollmentID: pending.EnrollmentID, RetryAfter: failure.delay()})
		if err := service.dependencies.Wait(ctx, failure.delay()); err != nil {
			return Result{}, err
		}
	}
	committed, err := service.dependencies.Store.Update(ctx, func(current *relayconfig.Config) error {
		next, err := pendingInstallation(*current, pending)
		if err == nil {
			*current = next
		}
		return err
	})
	if err != nil {
		service.dependencies.Log.Info("relay_installation_commit_failed", "operation_id", pending.OperationID,
			"enrollment_id", pending.EnrollmentID, "credential_id", pending.CredentialID, "reason", err)
		return Result{}, err
	}
	service.emit(progress, Progress{Stage: "installed", OperationID: pending.OperationID, EnrollmentID: pending.EnrollmentID})
	service.dependencies.Log.Info("relay_configuration_installed", "operation_id", pending.OperationID,
		"enrollment_id", pending.EnrollmentID, "credential_id", pending.CredentialID, "relay_url", pending.RelayURL)
	return installationResult(committed, pending.RelayURL, pending.CredentialID), nil
}

func (service *Service) requestEnrollment(ctx context.Context, saved relayconfig.Config) (relayconfig.Config, error) {
	pending := saved.Pending
	secret, err := base64.RawURLEncoding.DecodeString(pending.ClaimSecret)
	if err != nil || len(secret) != 32 {
		return relayconfig.Config{}, relayconfig.ErrConfig
	}
	digest := sha256.Sum256(secret)
	var response enrollmentResponse
	if err := service.call(ctx, http.MethodPost, endpoint(pending.ConsoleURL, "enrollments"),
		map[string]string{"device_name": pending.DeviceName, "claim_digest": encode(digest[:])}, &response); err != nil {
		return relayconfig.Config{}, err
	}
	parsed, parseErr := url.Parse(response.VerificationURL)
	console, _ := url.Parse(pending.ConsoleURL)
	if parseErr != nil || parsed.User != nil || parsed.Scheme != console.Scheme || parsed.Host != console.Host ||
		response.ID == "" || response.UserCode == "" || response.IntervalMS < 1_000 || response.IntervalMS > 30_000 ||
		response.ExpiresAt <= service.dependencies.Now().UnixMilli() {
		return relayconfig.Config{}, errors.New("invalid enrollment response")
	}
	return service.checkpoint(ctx, pending, func(current *relayconfig.PendingSetup) {
		current.EnrollmentID, current.UserCode = response.ID, response.UserCode
		current.VerificationURL, current.ExpiresAt, current.IntervalMS = response.VerificationURL, response.ExpiresAt, response.IntervalMS
	})
}
