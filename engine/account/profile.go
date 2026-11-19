package account

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
)

type Nickname struct {
	Value        string `json:"value"`
	NextChangeAt int64  `json:"next_change_at"`
}

type Profile struct {
	Revision uint64   `json:"revision"`
	Nickname Nickname `json:"nickname,omitzero"`
	Email    string   `json:"email,omitempty"`
}

type PendingEmail struct {
	Address   string    `json:"address"`
	Challenge Challenge `json:"challenge"`
}

func (record *Record) sameAccount(other Record) bool {
	return record.Credential != nil && other.Credential != nil && record.Credential.Account == other.Credential.Account
}

func (record *Record) advanceAccountState(previous Record) {
	if record.sameAccount(previous) {
		record.Entitlement = serviceauthority.Advance(previous.Entitlement, record.Entitlement)
		if previous.Profile.Revision > record.Profile.Revision {
			record.Profile = previous.Profile
		}
	}
}

func (record *Record) mergeIssuance(current Record) {
	// A ticket refresh can race a profile write from another process. The authority
	// versions confirmed changes; the pending delivery belongs to the local workflow.
	record.advanceAccountState(current)
	if record.sameAccount(current) && current.PendingEmail.Address != record.Profile.Email {
		record.PendingEmail = current.PendingEmail
	} else {
		record.PendingEmail = PendingEmail{}
	}
}

func (service *Service) validEmailChallenge(challenge Challenge) bool {
	return challenge.ID != "" && len(challenge.ID) <= 128 && challenge.Mode == "email" && service.acceptsChallengeExpiry(challenge.ExpiresAt)
}

func (service *Service) SetNickname(ctx context.Context, accountID, nickname string) error {
	return service.editAccount(ctx, accountID, func(record Record) error {
		return service.submitProfile(ctx, record, "/v1/accounts/profile/nickname", struct {
			Nickname string `json:"nickname"`
		}{nickname})
	})
}

func (service *Service) RequestProfileEmail(ctx context.Context, accountID, address string) error {
	return service.editAccount(ctx, accountID, func(record Record) error {
		var pending PendingEmail
		err := service.callWithTimeout(ctx, "/v1/accounts/profile/email", record.Credential.Secret, struct {
			Email string `json:"email"`
		}{address}, &pending, EmailRequestTimeout)
		if err != nil {
			return err
		}
		if pending.Address == "" || !service.validEmailChallenge(pending.Challenge) {
			return errors.New("invalid email challenge")
		}
		return service.saveProfileEdit(ctx, record, func(current *Record) {
			current.PendingEmail = pending
		})
	})
}

func (service *Service) ConfirmProfileEmail(ctx context.Context, accountID, challengeID, code string) error {
	return service.editAccount(ctx, accountID, func(record Record) error {
		pending := record.PendingEmail
		if challengeID == "" || challengeID != pending.Challenge.ID {
			return errEmailCodeRejected
		}
		return service.submitProfile(ctx, record, "/v1/accounts/profile/email/verify", struct {
			Email     string `json:"email"`
			Challenge string `json:"challenge"`
			Code      string `json:"code"`
		}{pending.Address, challengeID, code})
	})
}

func (service *Service) submitProfile(ctx context.Context, record Record, path string, input any) error {
	var profile Profile
	err := service.call(ctx, path, record.Credential.Secret, input, &profile)
	if err != nil {
		remote, ok := errors.AsType[*fault](err)
		if !ok || remote.Kind != "nickname_cooldown" {
			return err
		}
		// Another device already changed the nickname. Publish its name and deadline with the refusal.
		profile = remote.Profile
	}
	if profile.Revision == 0 {
		return serviceauthority.ErrFormat
	}
	if saveErr := service.saveProfileEdit(ctx, record, func(current *Record) {
		if profile.Revision >= current.Profile.Revision {
			current.Profile = profile
		}
		if current.PendingEmail.Address == current.Profile.Email {
			current.PendingEmail = PendingEmail{}
		}
	}); saveErr != nil {
		return saveErr
	}
	return err
}

func (service *Service) saveProfileEdit(ctx context.Context, previous Record, apply func(*Record)) error {
	record, err := service.config.Store.Update(ctx, func(current Record) (Record, error) {
		if !sameIdentity(current, previous) {
			return Record{}, ErrSuperseded
		}
		apply(&current)
		return current, nil
	})
	if err != nil {
		return err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.adoptLocked(record)
	service.notifyLocked()
	return nil
}
