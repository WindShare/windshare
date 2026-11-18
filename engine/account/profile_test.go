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

func profileReply(t *testing.T, status int, body any) *http.Response {
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
			return profileReply(t, http.StatusOK, pending), nil
		case "/v1/accounts/ticket":
			return profileReply(t, http.StatusOK, issued), nil
		case "/v1/accounts/profile/email/verify":
			var input struct{ Email, Challenge, Code string }
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Email != target || input.Challenge != pending.Challenge.ID {
				t.Fatalf("unexpected confirmation: %+v, %v", input, err)
			}
			if input.Code != "delivered" {
				return profileReply(t, http.StatusBadRequest, fault{Kind: "email_code_rejected"}), nil
			}
			return profileReply(t, http.StatusOK, Profile{Revision: 2, Email: target}), nil
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
	if err := reopened.step(t.Context()); err != nil {
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
	if err := reopened.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	updated, _ := testService(t, config).Snapshot()
	if updated.Profile.Email != target || updated.Profile.Revision != 2 || updated.PendingEmail != (PendingEmail{}) {
		t.Fatalf("confirmation lost to stale refresh: %+v", updated)
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
			return profileReply(t, http.StatusOK, profile), nil
		}
		return profileReply(t, http.StatusTooManyRequests, fault{Kind: "nickname_cooldown", Profile: profile}), nil
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
