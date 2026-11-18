package account

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
)

func (service *Service) load(ctx context.Context) (Record, error) {
	return service.config.Store.Update(ctx, func(record Record) (Record, error) {
		issuer := base64.RawURLEncoding.EncodeToString(service.config.Root[:])
		if record.Issuer != "" && record.Issuer != issuer {
			return Record{}, errors.New("account store belongs to another issuer")
		}
		if record.Certificate != "" {
			if _, err := serviceauthority.VerifyGeneration(record.Certificate, service.config.Root); err != nil {
				return Record{}, err
			}
		}
		return record, nil
	})
}

func (service *Service) initialize(ctx context.Context) (Record, error) {
	return service.config.Store.Update(ctx, func(record Record) (Record, error) {
		issuer := base64.RawURLEncoding.EncodeToString(service.config.Root[:])
		if record.Issuer != "" && record.Issuer != issuer {
			return Record{}, serviceauthority.ErrSignature
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
	if record.Credential == nil || record.Denial != "" {
		return false
	}
	generation, err := serviceauthority.VerifyGeneration(record.Certificate, service.config.Root)
	if err != nil {
		return false
	}
	ticket, err := serviceauthority.VerifyTicket(record.Ticket, generation, service.config.Now().Add(TicketMargin))
	return err == nil && ticket.Account == record.Credential.Account && ticket.Credential == record.Credential.ID
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

func (service *Service) install(ctx context.Context, previous Record, response session, login bool) error {
	generation, err := serviceauthority.VerifyGeneration(response.Certificate, service.config.Root)
	if err != nil {
		return err
	}
	ticket, err := serviceauthority.VerifyTicket(response.Ticket, generation, service.config.Now())
	if err != nil {
		return err
	}
	if ticket.Account != response.Account || ticket.Credential != response.Credential ||
		ticket.Entitlement != response.Entitlement || ticket.ExpiresAt != response.ExpiresAt {
		return serviceauthority.ErrSignature
	}
	if !login && previous.Credential != nil &&
		(ticket.Account != previous.Credential.Account || ticket.Credential != previous.Credential.ID) {
		return serviceauthority.ErrSignature
	}
	secret, err := issuedSecret(previous, response.Secret, login)
	if err != nil {
		return err
	}
	record, err := service.config.Store.Update(ctx, func(current Record) (Record, error) {
		if !sameIdentity(current, previous) {
			return Record{}, ErrSuperseded
		}
		if err := service.checkGeneration(current.Certificate, generation); err != nil {
			return Record{}, err
		}
		next := Record{Issuer: base64.RawURLEncoding.EncodeToString(service.config.Root[:]),
			Credential:  &Credential{Account: response.Account, ID: response.Credential, Secret: secret, Username: response.Username},
			Certificate: response.Certificate, Ticket: response.Ticket, Entitlement: response.Entitlement, Profile: response.Profile}
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
	service.mu.Unlock()
	service.transition(Ready, "", service.config.Now().Add(RefreshInterval))
	return nil
}
