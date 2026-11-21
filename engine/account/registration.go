package account

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

const (
	workChallenge  = "proof_of_work"
	emailChallenge = "email"
	// Matches CHALLENGE_SECONDS in the authority's challenge policy.
	authorityChallengeLifetime = 15 * time.Minute
)

func (challenge Challenge) requiredState() State {
	if challenge.Mode == emailChallenge {
		return EmailRequired
	}
	return RegistrationRequired
}

// Automatic admission can issue a credential immediately. Once the authority asks
// for user action, even a saved solution from an interrupted run needs a new gesture.
func (service *Service) pendingRegistration(ctx context.Context, record Record) (*Proof, error) {
	if proof := record.Proof; proof != nil && proof.Challenge.ExpiresAt > uint64(service.config.Now().Unix()) {
		return proof, nil
	}
	challenge, err := service.submitRegistration(ctx, record, nil)
	if err != nil || challenge == nil {
		return nil, err
	}
	return &Proof{Challenge: *challenge}, nil
}

// Register runs one user-requested attempt through issuance or an email handoff.
// The operation lock also keeps a refresh or a double submission from starting a second solve.
func (service *Service) Register(ctx context.Context) (err error) {
	if service == nil {
		return ErrNotConfigured
	}
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	record, err := service.syncSelection(ctx, service.initialize, nil)
	if err != nil {
		return err
	}
	if record.Credential != nil {
		if service.validTicket(record) {
			service.mu.Lock()
			monitor := service.accountViews != 0
			service.mu.Unlock()
			service.transition(record, Ready, "", service.ticketRefreshAt(record, service.config.Now(), monitor))
			return nil
		}
		return ErrSuperseded
	}
	service.transition(record, Registering, "", time.Time{})
	defer func() {
		if err != nil {
			var challengeID string
			if record.Proof != nil {
				challengeID = record.Proof.Challenge.ID
			}
			slog.Warn("account_registration_failed", "challenge_id", challengeID, "error", err)
			if isLocalStoreError(err) {
				service.mu.Lock()
				service.recordStorageErrorLocked(err)
				service.mu.Unlock()
				return
			}
			service.transition(record, RegistrationRequired, "", time.Time{})
		}
	}()
	record.Proof, err = service.pendingRegistration(ctx, record)
	if err != nil || record.Proof == nil {
		return err
	}
	proof := *record.Proof
	if proof.Challenge.Mode == emailChallenge {
		service.transition(record, EmailRequired, "", time.Time{})
		return nil
	}
	if !proof.Challenge.validWork() {
		return errInvalidRegistrationChallenge
	}
	if proof.Solution == "" {
		slog.Info("account_registration_verifying", "challenge_id", proof.Challenge.ID)
		service.setRegistrationProgress(registrationWorkStart)
		proof.Solution, err = service.solve(ctx, proof.Challenge)
		if err != nil {
			return err
		}
		if err := service.saveProof(ctx, record, &proof); err != nil {
			return err
		}
		record.Proof = &proof
	}
	service.setRegistrationProgress(registrationSubmitting)
	slog.Info("account_registration_submitting", "challenge_id", proof.Challenge.ID)
	challenge, err := service.submitRegistration(ctx, record, &proof)
	if err != nil || challenge == nil {
		return err
	}
	if challenge.Mode == emailChallenge {
		service.transition(record, EmailRequired, "", time.Time{})
		return nil
	}
	// A changed policy or source can invalidate completed work. Let the user retry
	// against the newly saved challenge instead of repeatedly consuming their CPU.
	return errors.New("account setup changed, try creating the account again")
}

// A successful issuance and a new challenge are distinct outcomes for both callers.
func (service *Service) submitRegistration(ctx context.Context, record Record, proof *Proof) (*Challenge, error) {
	request := struct {
		Secret    string  `json:"client_secret"`
		Challenge *string `json:"challenge"`
		Solution  *string `json:"solution"`
	}{Secret: record.SetupSecret}
	if proof != nil && proof.Solution != "" {
		request.Challenge, request.Solution = &proof.Challenge.ID, &proof.Solution
	}
	err := service.requestIssuance(ctx, record, "/v1/accounts", "", request)
	failure, ok := errors.AsType[*fault](err)
	if !ok || failure.Kind != "verification" {
		return nil, err
	}
	challenge := failure.Challenge
	if !service.acceptsRegistrationPrompt(challenge, proof) {
		return nil, errInvalidRegistrationChallenge
	}
	if err := service.saveProof(ctx, record, &Proof{Challenge: challenge}); err != nil {
		return nil, err
	}
	slog.Debug("account_registration_challenge_received", "challenge_id", challenge.ID, "mode", challenge.Mode, "expires_at", challenge.ExpiresAt)
	return &challenge, nil
}

func (service *Service) acceptsRegistrationPrompt(challenge Challenge, submitted *Proof) bool {
	if challenge.Mode == emailChallenge && challenge.ID != "" {
		// Only the email endpoint issues a code. A registration reply can echo that
		// challenge, even if its original deadline has passed on the local clock.
		return submitted != nil && submitted.Solution != "" && challenge == submitted.Challenge
	}
	return (challenge.validWork() || challenge.Mode == emailChallenge && challenge.Bits == 0) &&
		service.acceptsChallengeExpiry(challenge.ExpiresAt)
}

func (challenge Challenge) hasEmailCode(now time.Time) bool {
	return challenge.Mode == emailChallenge && challenge.ID != "" && challenge.ExpiresAt > uint64(now.Unix())
}

// The authority stamps expiry with its own clock, which can run ahead of the client.
func (service *Service) acceptsChallengeExpiry(expiresAt uint64) bool {
	now := uint64(service.config.Now().Unix())
	return expiresAt > now && expiresAt <= now+uint64(authorityChallengeLifetime/time.Second)+serviceauthority.ClockSkew
}
