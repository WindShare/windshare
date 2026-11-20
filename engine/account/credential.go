package account

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

type credentialSession struct {
	record     Record
	references uint32
	refreshing bool
	retry      time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
}

func (service *Service) retainCredentialLocked(record Record) *credentialSession {
	if record.Credential == nil {
		return nil
	}
	if service.credentials == nil {
		service.credentials = make(map[[16]byte]*credentialSession)
	}
	id := record.Credential.ID
	current := service.credentials[id]
	if current == nil {
		ctx, cancel := context.WithCancel(service.ctx)
		current = &credentialSession{
			record: cloneRecord(service.withDenialLocked(record)),
			ctx:    ctx,
			cancel: cancel,
		}
		service.credentials[id] = current
	}
	current.references++
	return current
}

func (share *ShareAuthorization) releaseCredentialLocked() {
	current := share.credential
	if current == nil {
		return
	}
	share.credential = nil
	current.references--
	if current.references == 0 {
		if current.cancel != nil {
			current.cancel()
		}
		delete(share.service.credentials, current.record.Credential.ID)
	}
}

func (service *Service) currentGenerationLocked(record Record) bool {
	if service.record.Certificate == "" {
		return true
	}
	latest, err := serviceauthority.VerifyGeneration(service.record.Certificate, service.config.Root)
	if err != nil {
		return false
	}
	candidate, err := serviceauthority.VerifyGeneration(record.Certificate, service.config.Root)
	return err == nil && serviceauthority.Compare(latest, candidate) == nil
}

func (service *Service) refreshCredentialLocked(current *credentialSession) {
	if current == nil || current.refreshing || current.references == 0 || service.ctx.Err() != nil {
		return
	}
	current.refreshing = true
	previous := cloneRecord(current.record)
	ctx := current.ctx
	if ctx == nil {
		ctx = service.ctx
	}
	service.workers.Go(func() {
		service.runCredentialRefresh(ctx, current, previous)
	})
}

func (service *Service) runCredentialRefresh(ctx context.Context, current *credentialSession, previous Record) {
	delay := service.executeCredentialRefresh(ctx, current, previous)
	if delay <= 0 {
		return
	}
	// Hold refreshing across the backoff window and signal on expiry so waiting
	// shares re-enter authorization without spinning or stalling indefinitely.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	service.mu.Lock()
	current.refreshing = false
	if current.references > 0 && ctx.Err() == nil {
		service.notifyLocked()
	}
	service.mu.Unlock()
}

func (service *Service) executeCredentialRefresh(ctx context.Context, current *credentialSession, previous Record) time.Duration {
	record, generation, err := service.refreshRetained(ctx, previous)
	if err == nil {
		err = service.lockLocal(ctx)
		if err == nil {
			defer func() { <-service.local }()
			record, err = service.persistRetained(ctx, record, generation)
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.applyCredentialRefreshLocked(current, record, err)
}

func (service *Service) applyCredentialRefreshLocked(current *credentialSession, record Record, err error) time.Duration {
	if err == nil && !service.currentGenerationLocked(record) {
		err = serviceauthority.ErrSignature
	}
	if err == nil {
		service.clearIssuedDenialLocked(record)
		service.record.Certificate = record.Certificate
		if sameIdentity(service.record, record) {
			service.adoptLocked(record)
		}
		current.record = service.withDenialLocked(record)
		if current.record.Denial != "" || service.validTicket(current.record) {
			current.refreshing = false
			current.retry = 0
			slog.Debug("account_retained_credential_refreshed", "credential", current.record.Credential.ID)
			service.notifyLocked()
			return 0
		}
	} else if failure, ok := errors.AsType[*fault](err); ok && failure.Kind == "denied" {
		service.rememberDenialLocked(current.record, failure.Reason)
		current.refreshing = false
		current.retry = 0
		slog.Debug("account_retained_credential_denied", "credential", current.record.Credential.ID, "reason", failure.Reason)
		return 0
	}
	if current.references == 0 || service.ctx.Err() != nil || current.record.Denial != "" ||
		service.validTicket(current.record) && service.currentGenerationLocked(current.record) {
		current.refreshing = false
		current.retry = 0
		return 0
	}
	delay := current.nextRetryDelay(err)
	slog.Debug("account_retained_credential_refresh_backoff",
		"credential", current.record.Credential.ID, "retry_in", delay, "error", err)
	return delay
}

func (current *credentialSession) nextRetryDelay(err error) time.Duration {
	delay := max(current.retry, MinimumRetry)
	current.retry = min(delay*2, MaximumRetry)
	if remote, ok := errors.AsType[*fault](err); ok && remote.Kind == "rate_limited" {
		delay = max(delay, time.Duration(min(remote.RetryAfter, maxRateLimitSeconds))*time.Second)
	}
	return delay
}

// Renew a retained credential in place so an older share cannot perturb the
// active account's state machine or emit selection transitions.
func (service *Service) refreshRetained(ctx context.Context, previous Record) (Record, serviceauthority.Generation, error) {
	if previous.Credential == nil {
		return Record{}, serviceauthority.Generation{}, serviceauthority.ErrFormat
	}
	var result session
	if err := service.call(ctx, "/v1/accounts/ticket", previous.Credential.Secret, struct{}{}, &result); err != nil {
		return Record{}, serviceauthority.Generation{}, err
	}
	record, generation, err := service.verifySession(previous, result, false)
	if err != nil {
		return Record{}, serviceauthority.Generation{}, err
	}
	if err := service.checkGeneration(previous.Certificate, generation); err != nil {
		return Record{}, serviceauthority.Generation{}, err
	}
	record.mergeIssuance(previous)
	return record, generation, nil
}

func (service *Service) persistRetained(ctx context.Context, record Record, generation serviceauthority.Generation) (Record, error) {
	// Every live publication shares the issuer's rollback barrier, while the
	// selected login remains independent from credentials retained by old shares.
	_, err := service.config.Store.Update(ctx, func(selected Record) (Record, error) {
		if err := service.checkGeneration(selected.Certificate, generation); err != nil {
			return Record{}, err
		}
		if sameIdentity(selected, record) {
			record.mergeIssuance(selected)
			return record, nil
		}
		if selected.Credential != nil && selected.Credential.Account == record.Credential.Account && selected.Denial == "account_blocked" {
			selected.Denial = ""
		}
		selected.Certificate = record.Certificate
		return selected, nil
	})
	return record, err
}
