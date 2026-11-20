package account

import (
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
	released   chan struct{}
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
		current = &credentialSession{
			record:   cloneRecord(service.withDenialLocked(record)),
			released: make(chan struct{}),
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
		if current.released != nil {
			close(current.released)
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
	config := service.config
	config.Store = &MemoryStore{record: cloneRecord(current.record)}
	service.workers.Go(func() {
		service.runCredentialRefresh(current, config)
	})
}

func (service *Service) runCredentialRefresh(current *credentialSession, config Config) {
	delay := service.executeCredentialRefresh(current, config)
	if delay <= 0 {
		return
	}
	// Hold refreshing across the backoff window and signal on expiry so waiting
	// shares re-enter authorization without spinning or stalling indefinitely.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-service.ctx.Done():
	case <-current.released:
	case <-timer.C:
	}
	service.mu.Lock()
	current.refreshing = false
	if current.references > 0 && service.ctx.Err() == nil {
		service.notifyLocked()
	}
	service.mu.Unlock()
}

func (service *Service) executeCredentialRefresh(current *credentialSession, config Config) time.Duration {
	record, err := service.refreshRetained(config)
	if err == nil {
		err = service.lockLocal(service.ctx)
		if err == nil {
			defer func() { <-service.local }()
			record, err = service.persistRetained(record)
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
			service.notifyLocked()
			return 0
		}
	} else if failure, ok := errors.AsType[*fault](err); ok && failure.Kind == "denied" {
		service.rememberDenialLocked(current.record, failure.Reason)
		current.refreshing = false
		current.retry = 0
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

func (service *Service) refreshRetained(config Config) (Record, error) {
	isolated, err := New(config)
	if err != nil {
		return Record{}, err
	}
	defer isolated.Close()
	if _, err := isolated.RefreshOnce(service.ctx); err != nil {
		return Record{}, err
	}
	isolated.mu.Lock()
	record := cloneRecord(isolated.record)
	isolated.mu.Unlock()
	return record, nil
}

func (service *Service) persistRetained(record Record) (Record, error) {
	generation, err := serviceauthority.VerifyGeneration(record.Certificate, service.config.Root)
	if err != nil {
		return Record{}, err
	}
	// Every live publication shares the issuer's rollback barrier, while the
	// selected login remains independent from credentials retained by old shares.
	_, err = service.config.Store.Update(service.ctx, func(selected Record) (Record, error) {
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
