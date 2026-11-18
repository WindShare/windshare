package account

import (
	"context"
	"errors"
	"time"
)

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
	service.Retry()
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
	if err := service.call(ctx, "/v1/accounts/email", "", struct {
		Email string `json:"email"`
	}{email}, &challenge); err != nil {
		return err
	}
	if challenge.ID == "" || len(challenge.ID) > 128 || challenge.Mode != "email" ||
		challenge.ExpiresAt <= uint64(service.config.Now().Unix()) ||
		challenge.ExpiresAt > uint64(service.config.Now().Unix())+15*60 {
		return errors.New("invalid email challenge")
	}
	if err := service.saveProof(ctx, previous, &Proof{Challenge: challenge}); err != nil {
		return err
	}
	service.transition(EmailRequired, "", time.Time{})
	service.Retry()
	return nil
}

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
	proof := *previous.Proof
	proof.Solution = code
	if err := service.saveProof(ctx, previous, &proof); err != nil {
		return err
	}
	service.Retry()
	return nil
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
