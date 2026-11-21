package account

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

// ReloadSelection publishes the shared local selection to observers independently of authority operations.
func (service *Service) ReloadSelection(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, CacheLoadTimeout)
	defer cancel()
	_, err := service.syncSelection(ctx, service.load, nil)
	return err
}

func (service *Service) load(ctx context.Context) (Record, error) {
	record, err := service.config.Store.Load(ctx)
	if err != nil {
		return Record{}, err
	}
	issuer := base64.RawURLEncoding.EncodeToString(service.config.Root[:])
	if record.Issuer != "" && record.Issuer != issuer {
		return Record{}, fmt.Errorf("%w: account store belongs to another issuer", ErrInvalidState)
	}
	if record.Certificate != "" {
		service.mu.Lock()
		verified := service.verifiedCertificate == record.Certificate
		service.mu.Unlock()
		if !verified {
			if _, err := serviceauthority.VerifyGeneration(record.Certificate, service.config.Root); err != nil {
				return Record{}, fmt.Errorf("%w: %w", ErrInvalidState, err)
			}
			service.mu.Lock()
			service.verifiedCertificate = record.Certificate
			service.mu.Unlock()
		}
	}
	return record, nil
}

// syncSelection publishes the stored identity and binds any waiting shares to
// the same snapshot. A read already in flight may predate a newly queued share.
func (service *Service) syncSelection(ctx context.Context, read func(context.Context) (Record, error), capture func(Record)) (Record, error) {
	if err := service.lockLocal(ctx); err != nil {
		return Record{}, err
	}
	defer func() { <-service.local }()

	service.mu.Lock()
	waiting := maps.Clone(service.unconfirmedShares)
	service.mu.Unlock()
	record, err := read(ctx)
	if err != nil {
		if isLocalStoreError(err) {
			service.mu.Lock()
			service.recordStorageErrorLocked(err)
			service.mu.Unlock()
		}
		return Record{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	record = service.withDenialLocked(record)
	changed := service.state == StorageError || !reflect.DeepEqual(service.record, record)
	if changed {
		service.adoptLocked(record)
	}
	for share := range waiting {
		if share.attribution != attributionClosed {
			share.bindSelectionLocked(service.record)
			delete(service.unconfirmedShares, share)
			changed = true
		}
	}
	if capture != nil {
		capture(service.record)
	}
	if changed {
		service.notifyLocked()
	}
	return service.record, nil
}

func (service *Service) initialize(ctx context.Context) (Record, error) {
	return service.config.Store.Update(ctx, func(record Record) (Record, error) {
		issuer := base64.RawURLEncoding.EncodeToString(service.config.Root[:])
		if record.Issuer != "" && record.Issuer != issuer {
			return Record{}, fmt.Errorf("%w: %w", ErrInvalidState, serviceauthority.ErrSignature)
		}
		record.Issuer = issuer
		if record.Credential == nil && record.SetupSecret == "" {
			var secret [32]byte
			if _, err := rand.Read(secret[:]); err != nil {
				return Record{}, err
			}
			record.SetupSecret = base64.RawURLEncoding.EncodeToString(secret[:])
		}
		return record, nil
	})
}

func (service *Service) validTicket(record Record) bool {
	_, valid := service.usableTicket(record, service.config.Now())
	return valid
}

func (service *Service) usableTicket(record Record, now time.Time) (serviceauthority.Ticket, bool) {
	if record.Credential == nil || record.Denial != "" {
		return serviceauthority.Ticket{}, false
	}
	ticket, err := service.verifyTicket(record, now.Add(TicketMargin))
	return ticket, err == nil && record.ownsTicket(ticket)
}

func (service *Service) verifyTicket(record Record, now time.Time) (serviceauthority.Ticket, error) {
	generation, err := serviceauthority.VerifyGeneration(record.Certificate, service.config.Root)
	if err != nil {
		return serviceauthority.Ticket{}, err
	}
	return serviceauthority.VerifyTicket(record.Ticket, generation, now)
}

func (record Record) ownsTicket(ticket serviceauthority.Ticket) bool {
	return record.Credential != nil && ticket.Account == record.Credential.Account && ticket.Credential == record.Credential.ID
}

func sameIdentity(a, b Record) bool {
	if a.Credential == nil || b.Credential == nil {
		return a.Credential == nil && b.Credential == nil && a.SetupSecret == b.SetupSecret
	}
	return a.Credential.ID == b.Credential.ID && a.Credential.Secret == b.Credential.Secret
}

func issuedSecret(previous Record, secret string, login bool) (string, error) {
	expected := previous.SetupSecret
	if previous.Credential != nil {
		expected = previous.Credential.Secret
	}
	if secret == "" {
		secret = expected
	}
	if !login && secret != expected {
		return "", serviceauthority.ErrSignature
	}
	if raw, err := serviceauthority.DecodeBase64(secret, 32); err != nil || len(raw) != 32 {
		return "", serviceauthority.ErrFormat
	}
	return secret, nil
}

func (service *Service) checkGeneration(certificate string, next serviceauthority.Generation) error {
	if certificate == "" {
		return nil
	}
	cached, err := serviceauthority.VerifyGeneration(certificate, service.config.Root)
	if err != nil {
		return err
	}
	return serviceauthority.Compare(cached, next)
}

func (service *Service) verifySession(previous Record, response session, login bool) (Record, serviceauthority.Generation, error) {
	generation, err := serviceauthority.VerifyGeneration(response.Certificate, service.config.Root)
	if err != nil {
		return Record{}, serviceauthority.Generation{}, err
	}
	ticket, err := serviceauthority.VerifyTicket(response.Ticket, generation, service.config.Now())
	if err != nil {
		return Record{}, serviceauthority.Generation{}, err
	}
	if ticket.Account != response.Account || ticket.Credential != response.Credential ||
		ticket.Entitlement != response.Entitlement || ticket.ExpiresAt != response.ExpiresAt {
		return Record{}, serviceauthority.Generation{}, serviceauthority.ErrSignature
	}
	if !login && previous.Credential != nil &&
		(ticket.Account != previous.Credential.Account || ticket.Credential != previous.Credential.ID) {
		return Record{}, serviceauthority.Generation{}, serviceauthority.ErrSignature
	}
	secret, err := issuedSecret(previous, response.Secret, login)
	if err != nil {
		return Record{}, serviceauthority.Generation{}, err
	}
	record := Record{
		Issuer:      base64.RawURLEncoding.EncodeToString(service.config.Root[:]),
		Credential:  &Credential{Account: response.Account, ID: response.Credential, Secret: secret, Username: response.Username},
		Certificate: response.Certificate,
		Ticket:      response.Ticket,
		Entitlement: response.Entitlement,
		Profile:     response.Profile,
	}
	return record, generation, nil
}

func (service *Service) install(ctx context.Context, previous Record, response session, login bool) error {
	issued, generation, err := service.verifySession(previous, response, login)
	if err != nil {
		return err
	}
	if err := service.lockLocal(ctx); err != nil {
		return err
	}
	defer func() { <-service.local }()
	record, err := service.config.Store.Update(ctx, func(current Record) (Record, error) {
		if !sameIdentity(current, previous) {
			return Record{}, ErrSuperseded
		}
		if err := service.checkGeneration(current.Certificate, generation); err != nil {
			return Record{}, err
		}
		next := cloneRecord(issued)
		next.mergeIssuance(current)
		return next, nil
	})
	if err != nil {
		return err
	}
	service.mu.Lock()
	if !service.currentGenerationLocked(record) {
		service.mu.Unlock()
		return serviceauthority.ErrSignature
	}
	service.issuedLocked(record)
	next := service.ticketRefreshAt(record, service.config.Now(), service.accountViews != 0)
	service.mu.Unlock()
	service.transition(record, Ready, "", next)
	return nil
}

// recordDenial serializes denial persistence with local store projections while
// guaranteeing that an explicit refusal is remembered in memory even if the
// context expires before acquiring the local gate or writing to the store.
func (service *Service) recordDenial(ctx context.Context, record Record, reason string, update func(Record) (Record, error)) error {
	if err := service.lockLocal(ctx); err != nil {
		service.mu.Lock()
		service.rememberDenialLocked(record, reason)
		service.mu.Unlock()
		return err
	}
	defer func() { <-service.local }()
	service.mu.Lock()
	service.rememberDenialLocked(record, reason)
	service.mu.Unlock()
	_, err := service.config.Store.Update(ctx, update)
	return err
}
