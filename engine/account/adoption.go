package account

import (
	"log/slog"
	"time"
)

// Denials are remembered before persistence. Losing a cache write or loading an
// older record cannot turn an explicit refusal into anonymous admission.
func (service *Service) rememberDenialLocked(record Record, reason string) {
	if record.Credential == nil {
		// A lost issuance reply can leave a denied account identified only by its setup secret.
		if record.SetupSecret != "" && sameIdentity(service.record, record) {
			service.record.Denial = reason
		}
		return
	}
	if service.credentialDenials == nil {
		service.credentialDenials = make(map[[16]byte]string)
	}
	if service.accountDenials == nil {
		service.accountDenials = make(map[[16]byte]string)
	}
	if reason == "account_blocked" {
		service.accountDenials[record.Credential.Account] = reason
	} else {
		service.credentialDenials[record.Credential.ID] = reason
	}
	for _, retained := range service.credentials {
		retained.record = service.withDenialLocked(retained.record)
	}
	service.record = service.withDenialLocked(service.record)
	if service.record.Denial != "" {
		service.state, service.reason = Denied, service.record.Denial
	}
	service.notifyLocked()
}

func (service *Service) withDenialLocked(record Record) Record {
	if credential := record.Credential; credential != nil {
		if reason := service.credentialDenials[credential.ID]; reason != "" {
			record.Denial = reason
		}
		if reason := service.accountDenials[credential.Account]; reason != "" {
			record.Denial = reason
		}
	} else if record.SetupSecret != "" && record.Denial == "" && sameIdentity(service.record, record) {
		record.Denial = service.record.Denial
	}
	return record
}

func (service *Service) adoptLocked(record Record) {
	// File reads and retained credential refreshes finish independently. Preserve
	// the issuer barrier even when a slower read selects another local account.
	if !service.currentGenerationLocked(record) {
		record.Certificate, record.Ticket = service.record.Certificate, ""
	}
	record.advanceAccountState(service.record)
	if record.Denial != "" {
		service.rememberDenialLocked(record, record.Denial)
	}
	record = service.withDenialLocked(record)
	recoveredStorage := service.state == StorageError
	selectionChanged := !sameIdentity(service.record, record)
	service.record = record
	if selectionChanged || service.state == "" || recoveredStorage {
		// Progress and retry deadlines belong to the previous selection or local store failure.
		service.state, service.reason = service.cachedState(record)
		service.registrationPermille, service.retryAt = 0, time.Time{}
	}
	if selectionChanged || recoveredStorage {
		slog.Debug("account_selection_adopted", "state", service.state, "has_credential", record.Credential != nil)
		service.wakeRefreshLocked()
	}
	if record.Credential != nil {
		if retained := service.credentials[record.Credential.ID]; retained != nil {
			retained.record = cloneRecord(record)
		}
	}
}

func (service *Service) recordStorageErrorLocked(err error) {
	reason := err.Error()
	if service.state == StorageError && service.reason == reason {
		return
	}
	previous := service.state
	service.state, service.reason = StorageError, reason
	service.registrationPermille, service.retryAt = 0, time.Time{}
	service.notifyLocked()
	slog.Warn("account_storage_error", "from", previous, "reason", reason)
}

func (service *Service) cachedState(record Record) (State, string) {
	switch {
	case record.Denial != "":
		return Denied, record.Denial
	case record.Credential != nil:
		if service.validTicket(record) {
			return Ready, ""
		}
		return RefreshRequired, ""
	case record.Proof != nil:
		return record.Proof.Challenge.requiredState(), ""
	default:
		return Pending, ""
	}
}

// Only a verified, persisted issuance may release a denial. Polling, expiry and
// switching the currently selected account never clear the retained evidence.
func (service *Service) issuedLocked(record Record) {
	service.clearIssuedDenialLocked(record)
	service.adoptLocked(record)
}

func (service *Service) clearIssuedDenialLocked(record Record) {
	if record.Credential != nil {
		delete(service.credentialDenials, record.Credential.ID)
		delete(service.accountDenials, record.Credential.Account)
		if service.record.Credential != nil && service.record.Credential.Account == record.Credential.Account && service.record.Denial == "account_blocked" {
			service.record.Denial = ""
		}
		for _, retained := range service.credentials {
			if retained.record.Credential.Account == record.Credential.Account && retained.record.Denial == "account_blocked" {
				retained.record.Denial = ""
			}
		}
	}
}
