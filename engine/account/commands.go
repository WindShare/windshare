package account

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
)

var (
	errEmailCodeRejected = errors.New("verification code is invalid or expired; request a new verification email")
	// errEmailCodeIncorrect reports a mistyped code whose still-live challenge the authority returned for another attempt.
	errEmailCodeIncorrect = errors.New("verification code is incorrect; check it and try again")
)

// CommandErrorCode lets interactive clients localize a recovery action without parsing authority messages.
func CommandErrorCode(err error) string {
	switch {
	case errors.Is(err, errEmailCodeIncorrect):
		return "email_code_incorrect"
	case errors.Is(err, errEmailCodeRejected):
		return "email_code_rejected"
	case errors.Is(err, ErrSuperseded):
		return "account_changed"
	case isLocalStoreError(err):
		return "account_storage_error"
	}
	if remote, ok := errors.AsType[*fault](err); ok {
		switch remote.Kind {
		case "denied":
			switch remote.Reason {
			case "credentials":
				return "invalid_credentials"
			case "username_taken":
				return "username_taken"
			case "email_taken":
				return "email_taken"
			case "email_registration_disabled":
				return "email_registration_disabled"
			case "credentials_changed":
				return "account_changed"
			}
			return "account_denied"
		case "invalid":
			return "invalid_account_input"
		case "rate_limited", "invalid_nickname", "nickname_cooldown", "invalid_email", "unsupported_email", "email_code_rejected", "email_delivery_unavailable":
			return remote.Kind
		}
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return "account_service_unreachable"
	}
	return "account_operation_failed"
}

func (service *Service) Login(ctx context.Context, username, password string) error {
	if service == nil {
		return ErrNotConfigured
	}
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	previous, err := service.load(ctx)
	if err != nil {
		return err
	}
	var result session
	if err := service.call(ctx, "/v1/accounts/login", "", struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{username, password}, &result); err != nil {
		return err
	}
	return service.install(ctx, previous, result, true)
}

// Resolve the credential from the shared store because another process may have
// switched accounts while the caller was preparing the edit.
func (service *Service) editAccount(ctx context.Context, accountID string, submit func(Record) error) error {
	if service == nil {
		return ErrNotConfigured
	}
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	record, err := service.load(ctx)
	if err != nil {
		return err
	}
	if record.Credential == nil || base64.RawURLEncoding.EncodeToString(record.Credential.Account[:]) != accountID {
		return ErrSuperseded
	}
	return submit(record)
}

func (service *Service) SetPassword(ctx context.Context, accountID, username, password string) error {
	return service.editAccount(ctx, accountID, func(record Record) error {
		var result session
		if err := service.call(ctx, "/v1/accounts/password", record.Credential.Secret, struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}{username, password}, &result); err != nil {
			return err
		}
		return service.install(ctx, record, result, false)
	})
}

func (service *Service) RequestEmail(ctx context.Context, email string) error {
	if service == nil {
		return ErrNotConfigured
	}
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	previous, err := service.syncSelection(ctx, service.initialize, nil)
	if err != nil {
		return err
	}
	if previous.Credential != nil {
		return errors.New("account is already registered")
	}
	var challenge Challenge
	if err := service.callWithTimeout(ctx, "/v1/accounts/email", "", struct {
		Email        string `json:"email"`
		ClientSecret string `json:"client_secret"`
	}{Email: email, ClientSecret: previous.SetupSecret}, &challenge, EmailRequestTimeout); err != nil {
		return err
	}
	if !service.validEmailChallenge(challenge) {
		return errors.New("invalid email challenge")
	}
	// Issuing the email replaces the authority's previous challenge.
	if err := service.updateProof(ctx, previous, func(*Proof) (*Proof, error) {
		return &Proof{Challenge: challenge}, nil
	}); err != nil {
		return err
	}
	service.transition(previous, EmailRequired, "", time.Time{})
	return nil
}

// VerifyEmail completes registration with the authority before reporting success.
func (service *Service) VerifyEmail(ctx context.Context, code string) error {
	if service == nil {
		return ErrNotConfigured
	}
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	previous, err := service.load(ctx)
	if err != nil {
		return err
	}
	if previous.Proof == nil || previous.Proof.Challenge.Mode != "email" || previous.Proof.Challenge.ID == "" {
		return errors.New("request a verification email first")
	}
	if len(code) == 0 || len(code) > 128 {
		return errors.New("invalid verification code")
	}
	// The setup secret makes retries idempotent, so the submitted email code
	// can stay in memory while we wait for the authority's decision.
	proof := *previous.Proof
	proof.Solution = code
	challenge, err := service.submitRegistration(ctx, previous, &proof)
	if err != nil || challenge == nil {
		return err
	}
	if *challenge != proof.Challenge && service.config.Diagnostics.Enabled() {
		service.emitDiagnostic("account_email_verification_challenge_replaced", diagnostics.Fields{
			"challenge_id": diagnostics.Text(proof.Challenge.ID),
			"next_mode":    diagnostics.Text(challenge.Mode),
		})
	}
	service.transition(previous, challenge.requiredState(), "", time.Time{})
	if challenge.Mode == emailChallenge {
		// A reply can cross the locally visible deadline while the authority still accepts the code.
		if *challenge == proof.Challenge && challenge.hasEmailCode(service.config.Now()) {
			return errEmailCodeIncorrect
		}
		return errEmailCodeRejected
	}
	return errors.New("registration requirements changed; retry account setup")
}

func (service *Service) RevokeCredential(ctx context.Context, accountID string, credential [16]byte) error {
	return service.editAccount(ctx, accountID, func(previous Record) error {
		var result struct{}
		if err := service.call(ctx, "/v1/accounts/credentials/revoke", previous.Credential.Secret,
			struct {
				Credential [16]byte `json:"credential"`
			}{credential}, &result); err != nil {
			return err
		}
		target := Record{Credential: &Credential{ID: credential, Account: previous.Credential.Account}}
		return service.recordDenial(ctx, target, denialCredentialRevoked, func(current Record) (Record, error) {
			if current.Credential != nil && current.Credential.ID == credential {
				current.Denial = denialCredentialRevoked
			}
			return current, nil
		})
	})
}
