package account

import (
	"time"

	"github.com/windshare/windshare/core/diagnostics"
)

const (
	denialAccountBlocked    = "account_blocked"
	denialCredentialRevoked = "credential_revoked"
)

// A non-login issuance cannot clear a permanent credential revocation or a denial
// recorded after the refresh started, while a retry that began from a reversible
// account block can still restore access after an unblock.
func supersededByDenial(previous, current string) bool {
	return current == denialCredentialRevoked || current != "" && current != previous
}

func deniedIssuance(previous, stored, memory string) (string, bool) {
	if supersededByDenial(previous, stored) {
		return stored, true
	}
	if supersededByDenial(previous, memory) {
		return memory, true
	}
	return "", false
}

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
	if reason == denialAccountBlocked {
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

func (service *Service) memoryDenialLocked(previous, issued Record) string {
	if credential := issued.Credential; credential != nil {
		if reason := service.credentialDenials[credential.ID]; reason == denialCredentialRevoked {
			return reason
		}
		if reason := service.accountDenials[credential.Account]; reason != "" {
			return reason
		}
		if reason := service.credentialDenials[credential.ID]; reason != "" {
			return reason
		}
	}
	if previous.Credential == nil && previous.SetupSecret != "" && sameIdentity(service.record, previous) {
		return service.record.Denial
	}
	return ""
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
	previousState := service.state
	recoveredStorage := service.state == StorageError
	if record.Denial != "" {
		service.rememberDenialLocked(record, record.Denial)
	}
	record = service.withDenialLocked(record)
	selectionChanged := !sameIdentity(service.record, record)
	service.record = record
	if selectionChanged || service.state == "" || recoveredStorage {
		// Progress and retry deadlines belong to the previous selection or local store failure.
		service.state, service.reason = service.cachedState(record)
		service.registrationPermille, service.retryAt = 0, time.Time{}
	}
	if previousState != "" && previousState != service.state {
		service.emitStateChanged(previousState, service.state, service.reason, time.Time{}, record.Credential)
	}
	if selectionChanged || recoveredStorage {
		if service.config.Diagnostics.Enabled() {
			fields := diagnostics.Fields{
				"state":          diagnostics.Text(string(service.state)),
				"has_credential": diagnostics.Bool(record.Credential != nil),
			}
			credentialFields(fields, record.Credential)
			service.emitDiagnostic("account_selection_adopted", fields)
		}
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
	if service.config.Diagnostics.Enabled() {
		service.emitDiagnostic("account_storage_error", diagnostics.Fields{
			"from":   diagnostics.Text(string(previous)),
			"reason": diagnostics.Text(reason),
		})
	}
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
		if service.record.Credential != nil && service.record.Credential.Account == record.Credential.Account && service.record.Denial == denialAccountBlocked {
			service.record.Denial = ""
		}
		for _, retained := range service.credentials {
			if retained.record.Credential.Account == record.Credential.Account && retained.record.Denial == denialAccountBlocked {
				retained.record.Denial = ""
			}
		}
	}
}
