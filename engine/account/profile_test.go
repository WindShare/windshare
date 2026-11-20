package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func accountReply(t *testing.T, status int, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data))}
}

func TestProfileEmailRequiresDeliveredCodeAndSurvivesRestartAndRefresh(t *testing.T) {
	config, issued := accountFixture(t)
	config.Store = FileStore{Path: filepath.Join(t.TempDir(), "account.json")}
	issued.Profile = Profile{Revision: 1, Email: "old@qq.com"}
	record := cachedRecord(issued)
	record.Profile = issued.Profile
	if _, err := config.Store.Update(t.Context(), func(Record) (Record, error) { return record, nil }); err != nil {
		t.Fatal(err)
	}
	const target = "new@163.com"
	pending := PendingEmail{Address: target, Challenge: Challenge{ID: "delivery", Mode: "email", ExpiresAt: uint64(config.Now().Unix()) + 900}}
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+issued.Secret {
			t.Fatal("profile request omitted selected credential")
		}
		switch request.URL.Path {
		case "/v1/accounts/profile/email":
			var input struct{ Email string }
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Email != target {
				t.Fatalf("unexpected delivery request: %+v, %v", input, err)
			}
			return accountReply(t, http.StatusOK, pending), nil
		case "/v1/accounts/ticket":
			return accountReply(t, http.StatusOK, issued), nil
		case "/v1/accounts/profile/email/verify":
			var input struct{ Email, Challenge, Code string }
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Email != target || input.Challenge != pending.Challenge.ID {
				t.Fatalf("unexpected confirmation: %+v, %v", input, err)
			}
			if input.Code != "delivered" {
				return accountReply(t, http.StatusBadRequest, fault{Kind: "email_code_rejected"}), nil
			}
			return accountReply(t, http.StatusOK, Profile{Revision: 2, Email: target}), nil
		default:
			t.Fatalf("unexpected route %s", request.URL.Path)
			return nil, nil
		}
	})
	service := testService(t, config)
	start, _ := service.Snapshot()
	if err := service.RequestProfileEmail(t.Context(), start.Account, target); err != nil {
		t.Fatal(err)
	}
	reopened := testService(t, config)
	if _, err := reopened.RefreshOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ConfirmProfileEmail(t.Context(), start.Account, pending.Challenge.ID, "wrong"); CommandErrorCode(err) != "email_code_rejected" {
		t.Fatalf("invalid code: %v", err)
	}
	waiting, _ := reopened.Snapshot()
	if waiting.Profile != issued.Profile || waiting.PendingEmail != pending || waiting.EmailCodePending {
		t.Fatalf("unverified email replaced profile or registration state: %+v", waiting)
	}
	if err := reopened.ConfirmProfileEmail(t.Context(), start.Account, pending.Challenge.ID, "delivered"); err != nil {
		t.Fatal(err)
	}
	// A delayed issuance carries the previously verified address.
	if _, err := reopened.RefreshOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	updated, _ := testService(t, config).Snapshot()
	if updated.Profile.Email != target || updated.Profile.Revision != 2 || updated.PendingEmail != (PendingEmail{}) {
		t.Fatalf("confirmation lost to stale refresh: %+v", updated)
	}
}

func TestPasswordEditRejectsAnAccountSwitchBeforeSending(t *testing.T) {
	config, selected := accountFixture(t)
	replacement := otherAccount(t, config, selected)
	replacement.Username = "new-login"
	store := &MemoryStore{record: cachedRecord(selected)}
	config.Store = store
	calls := 0
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/v1/accounts/password" || request.Header.Get("Authorization") != "Bearer "+replacement.Secret {
			t.Fatalf("unexpected login edit request: %s", request.URL.Path)
		}
		return accountReply(t, http.StatusOK, replacement), nil
	})
	service := testService(t, config)
	opened, _ := service.Snapshot()

	// Another process selects a new account while this process still shows the old one.
	if _, err := store.Update(t.Context(), func(Record) (Record, error) {
		return cachedRecord(replacement), nil
	}); err != nil {
		t.Fatal(err)
	}
	err := service.SetPassword(t.Context(), opened.Account, "new-login", "new-password")
	if !errors.Is(err, ErrSuperseded) || calls != 0 {
		t.Fatalf("stale edit sent an account write: err = %v, requests = %d", err, calls)
	}

	current, _ := testService(t, config).Snapshot()
	if err := service.SetPassword(t.Context(), current.Account, "new-login", "new-password"); err != nil {
		t.Fatal(err)
	}
	updated, _ := service.Snapshot()
	if calls != 1 || updated.Account != current.Account || updated.Username != "new-login" {
		t.Fatalf("login edit did not update the selected account: requests = %d, account = %s, username = %s", calls, updated.Account, updated.Username)
	}
}

func TestRevokeCredentialRejectsAnAccountSwitchBeforeSending(t *testing.T) {
	config, selected := accountFixture(t)
	replacement := otherAccount(t, config, selected)
	store := &MemoryStore{record: cachedRecord(selected)}
	config.Store = store
	calls := 0
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/v1/accounts/credentials/revoke" || request.Header.Get("Authorization") != "Bearer "+replacement.Secret {
			t.Fatalf("unexpected revoke request: %s", request.URL.Path)
		}
		return accountReply(t, http.StatusOK, struct{}{}), nil
	})
	service := testService(t, config)
	opened, _ := service.Snapshot()

	// Another process switches the selected account while this process still shows the old one.
	if _, err := store.Update(t.Context(), func(Record) (Record, error) {
		return cachedRecord(replacement), nil
	}); err != nil {
		t.Fatal(err)
	}
	err := service.RevokeCredential(t.Context(), opened.Account, selected.Credential)
	if !errors.Is(err, ErrSuperseded) || CommandErrorCode(err) != "account_changed" || calls != 0 {
		t.Fatalf("stale revoke contacted the authority: err = %v, requests = %d", err, calls)
	}

	current, _ := testService(t, config).Snapshot()
	if err := service.RevokeCredential(t.Context(), current.Account, replacement.Credential); err != nil {
		t.Fatal(err)
	}
	stored, err := service.load(t.Context())
	if err != nil || calls != 1 || stored.Denial != "credential_revoked" {
		t.Fatalf("revoke did not persist denial: calls = %d, denial = %q, err = %v", calls, stored.Denial, err)
	}
}

func TestNicknameCooldownSynchronizesAuthorityProfileWithoutSwitchingAccount(t *testing.T) {
	config, issued := accountFixture(t)
	config.Store = &MemoryStore{record: cachedRecord(issued)}
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct{ Refreshed session }
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	profile := vector.Refreshed.Profile
	if profile.Nickname.Value != "春风" || profile.Nickname.NextChangeAt <= config.Now().Unix() {
		t.Fatalf("missing authority nickname contract: %+v", profile)
	}
	calls := 0
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var input struct{ Nickname string }
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || request.URL.Path != "/v1/accounts/profile/nickname" || request.Header.Get("Authorization") != "Bearer "+issued.Secret {
			t.Fatalf("unexpected nickname request: %s, %v", request.URL.Path, err)
		}
		if input.Nickname == profile.Nickname.Value {
			return accountReply(t, http.StatusOK, profile), nil
		}
		return accountReply(t, http.StatusTooManyRequests, fault{Kind: "nickname_cooldown", Profile: profile}), nil
	})
	service := testService(t, config)
	before, _ := service.Snapshot()
	if err := service.SetNickname(t.Context(), "previous-account", "another"); !errors.Is(err, ErrSuperseded) || calls != 0 {
		t.Fatalf("sent change against a different account: %v", err)
	}
	if err := service.SetNickname(t.Context(), before.Account, "another"); CommandErrorCode(err) != "nickname_cooldown" {
		t.Fatalf("cooldown: %v", err)
	}
	cached, _ := testService(t, config).Snapshot()
	if cached.Profile != profile || cached.Credential != before.Credential || cached.Username != before.Username {
		t.Fatal("cooldown lost the authoritative profile or changed login")
	}
	if err := service.SetNickname(t.Context(), before.Account, profile.Nickname.Value); err != nil {
		t.Fatalf("retry of saved name: %v", err)
	}
}

func TestCommandErrorCodeMapsEmailDenials(t *testing.T) {
	if got := CommandErrorCode(&fault{Kind: "denied", Reason: "email_taken"}); got != "email_taken" {
		t.Fatalf("email_taken mapped to %q", got)
	}
	if got := CommandErrorCode(&fault{Kind: "denied", Reason: "email_registration_disabled"}); got != "email_registration_disabled" {
		t.Fatalf("email_registration_disabled mapped to %q", got)
	}
}
