package account

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"time"
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
	}
	if remote, ok := errors.AsType[*fault](err); ok {
		switch remote.Kind {
		case "denied":
			switch remote.Reason {
			case "credentials":
				return "invalid_credentials"
			case "username_taken":
				return "username_taken"
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
	if err := service.install(ctx, previous, result, true); err != nil {
		return err
	}
	service.Start()
	return nil
}

func (service *Service) SetPassword(ctx context.Context, username, password string) error {
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
	if previous.Credential == nil {
		return errors.New("account setup is still pending")
	}
	var result session
	if err := service.call(ctx, "/v1/accounts/password", previous.Credential.Secret, struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{username, password}, &result); err != nil {
		return err
	}
	return service.install(ctx, previous, result, false)
}

func (service *Service) RequestEmail(ctx context.Context, email string) error {
	if service == nil {
		return ErrNotConfigured
	}
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	previous, err := service.initialize(ctx)
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
	service.transition(EmailRequired, "", time.Time{})
	service.Retry()
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
	if *challenge != proof.Challenge {
		slog.Debug("account_email_verification_challenge_replaced", "challenge_id", proof.Challenge.ID, "next_mode", challenge.Mode)
	}
	service.transition(challenge.requiredState(), "", time.Time{})
	if challenge.Mode == emailChallenge {
		// A reply can cross the locally visible deadline while the authority still accepts the code.
		if *challenge == proof.Challenge && challenge.hasEmailCode(service.config.Now()) {
			return errEmailCodeIncorrect
		}
		return errEmailCodeRejected
	}
	return errors.New("registration requirements changed; retry account setup")
}

func (service *Service) RevokeCredential(ctx context.Context, credential [16]byte) error {
	if service == nil {
		return ErrNotConfigured
	}
	previous, err := service.load(ctx)
	if err != nil {
		return err
	}
	if previous.Credential == nil {
		return errors.New("account setup is still pending")
	}
	var result struct{}
	err = service.call(ctx, "/v1/accounts/credentials/revoke", previous.Credential.Secret,
		struct {
			Credential [16]byte `json:"credential"`
		}{credential}, &result)
	if err != nil {
		return err
	}
	service.mu.Lock()
	service.rememberDenialLocked(Record{Credential: &Credential{ID: credential, Account: previous.Credential.Account}}, "credential_revoked")
	service.mu.Unlock()
	_, err = service.config.Store.Update(ctx, func(current Record) (Record, error) {
		if current.Credential != nil && current.Credential.ID == credential {
			current.Denial = "credential_revoked"
		}
		return current, nil
	})
	service.Retry()
	return err
}
