package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestWorkRegistrationRequiresUserAfterDiscoveryAndRestart(t *testing.T) {
	config, issued := accountFixture(t)
	challenge := Challenge{ID: "registration", Mode: workChallenge, Bits: 4, ExpiresAt: uint64(config.Now().Unix()) + 60}
	requests := 0
	var secret, solution string
	var service *Service
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		var input struct {
			Secret              string `json:"client_secret"`
			Challenge, Solution string
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || request.URL.Path != "/v1/accounts" {
			t.Fatal("unexpected registration request", err)
		}
		switch requests {
		case 1:
			secret = input.Secret
			if secret == "" || input.Solution != "" {
				t.Fatal("first contact must only discover admission")
			}
			return accountReply(t, http.StatusPreconditionRequired, fault{Kind: "verification", Challenge: challenge}), nil
		case 2:
			if input.Secret != secret || input.Challenge != challenge.ID || !proofMatches([]byte(challenge.ID+input.Solution), challenge.Bits) {
				t.Fatal("explicit registration did not answer the saved challenge")
			}
			solution = input.Solution
			return nil, errors.New("reply lost")
		case 3:
			if input.Secret != secret || input.Solution != solution {
				t.Fatal("explicit retry discarded completed work")
			}
			status, _ := service.Snapshot()
			if status.State != Registering || status.RegistrationPermille != registrationSubmitting || status.Account != "" {
				t.Fatal("reported completion before issuance", status.State)
			}
			issued.Secret = secret
			return accountReply(t, http.StatusOK, issued), nil
		default:
			t.Fatalf("unexpected request %d", requests)
			return nil, nil
		}
	})
	service = testService(t, config)
	if err := service.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	// First share, cache polling and a process restart must all stop at the same boundary.
	for _, expectedRequests := range []int{1, 2} {
		service = testService(t, config)
		if err := service.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		status, _ := service.Snapshot()
		if status.State != RegistrationRequired || status.RegistrationPermille != 0 || requests != expectedRequests {
			t.Fatalf("passive refresh advanced registration: state=%s requests=%d", status.State, requests)
		}
		err := service.Register(t.Context())
		if (err != nil) != (expectedRequests == 1) {
			t.Fatalf("attempt %d: %v", expectedRequests, err)
		}
	}
	if status, _ := service.Snapshot(); status.State != Ready || status.Account == "" || requests != 3 {
		t.Fatal("credential not installed after explicit retry")
	}
	if err := service.Register(t.Context()); err != nil || requests != 3 {
		t.Fatal("duplicate submit issued another account", err)
	}
}

func TestRegisterNegotiatesFromSettingsAndRenewsExpiredWork(t *testing.T) {
	for _, expired := range []bool{false, true} {
		config, issued := accountFixture(t)
		now := uint64(config.Now().Unix())
		if expired {
			config.Store = &MemoryStore{record: Record{SetupSecret: issued.Secret, Proof: &Proof{
				Challenge: Challenge{ID: "old", Mode: workChallenge, Bits: 4, ExpiresAt: now}, Solution: "old-solution",
			}}}
		}
		requests := 0
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			var input struct {
				Secret              string `json:"client_secret"`
				Challenge, Solution string
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if requests == 1 {
				if input.Challenge != "" || input.Solution != "" {
					t.Fatal("submitted an expired solution")
				}
				challenge := Challenge{ID: "new", Mode: workChallenge, Bits: 4, ExpiresAt: now + 60}
				if expired {
					challenge = Challenge{Mode: emailChallenge, ExpiresAt: now + 60}
				}
				return accountReply(t, http.StatusPreconditionRequired, fault{Kind: "verification", Challenge: challenge}), nil
			}
			if expired || requests != 2 || input.Challenge != "new" || input.Solution == "" {
				t.Fatal("unexpected submission after discovering requirements")
			}
			issued.Secret = input.Secret
			return accountReply(t, http.StatusOK, issued), nil
		})
		service := testService(t, config)
		if err := service.Register(t.Context()); err != nil {
			t.Fatal(err)
		}
		status, _ := service.Snapshot()
		if expired && (requests != 1 || status.State != EmailRequired) ||
			!expired && (requests != 2 || status.State != Ready) {
			t.Fatalf("expired=%t state=%s requests=%d", expired, status.State, requests)
		}
	}
}

func TestRegistrationEstimateReservesCompletionForIssuance(t *testing.T) {
	previous := estimatedRegistrationPermille(0, 28)
	for _, attempts := range []uint64{1 << 24, 1 << 27, 1 << 28, 1 << 30, 1 << 50} {
		next := estimatedRegistrationPermille(attempts, 28)
		if next <= previous || next >= registrationSubmitting {
			t.Fatalf("estimate for %d attempts went from %d to %d", attempts, previous, next)
		}
		previous = next
	}
}

func TestExplicitRegistrationPreservesDenialBeforeCredentialRecovery(t *testing.T) {
	config, issued := accountFixture(t)
	config.Store = &rejectingDenialStore{record: Record{SetupSecret: issued.Secret}}
	config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
		return accountReply(t, http.StatusForbidden, fault{Kind: "denied", Reason: "account_blocked"}), nil
	})
	service := testService(t, config)
	if err := service.Register(t.Context()); CommandErrorCode(err) != "account_denied" {
		t.Fatal(err)
	}
	current, err := service.load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.adoptLocked(current)
	service.mu.Unlock()
	if status, _ := service.Snapshot(); status.State != Denied || status.Reason != "account_blocked" || service.record.Denial == "" {
		t.Fatalf("registration denial lost: %s %s", status.State, status.Reason)
	}
}
