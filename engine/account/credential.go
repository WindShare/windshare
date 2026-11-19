package account

import (
	"errors"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

type credentialSession struct {
	record     Record
	references uint32
	refreshing bool
	retryAt    time.Time
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
		current = &credentialSession{record: cloneRecord(service.withDenialLocked(record))}
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
	if current == nil || current.refreshing || service.ctx.Err() != nil ||
		service.config.Now().Before(current.retryAt) {
		return
	}
	current.refreshing = true
	config := service.config
	config.Store = &MemoryStore{record: cloneRecord(current.record)}
	service.workers.Go(func() {
		record, err := service.refreshRetained(config)
		service.mu.Lock()
		defer service.mu.Unlock()
		if err == nil && !service.currentGenerationLocked(record) {
			err = serviceauthority.ErrSignature
		}
		current.refreshing = false
		current.retryAt = service.config.Now().Add(MaximumRetry)
		if err == nil {
			service.clearIssuedDenialLocked(record)
			service.record.Certificate = record.Certificate
			if sameIdentity(service.record, record) {
				service.adoptLocked(record)
			}
			current.record = service.withDenialLocked(record)
		} else {
			var failure *fault
			if errors.As(err, &failure) && failure.Kind == "denied" {
				service.rememberDenialLocked(current.record, failure.Reason)
			}
		}
		service.notifyLocked()
	})
}

func (service *Service) refreshRetained(config Config) (Record, error) {
	isolated, err := New(config)
	if err != nil {
		return Record{}, err
	}
	defer isolated.Close()
	if err := isolated.step(service.ctx); err != nil {
		return Record{}, err
	}
	record := isolated.record
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
