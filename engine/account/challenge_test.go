package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

// The authority stamps expiry from its own clock. A client a few seconds behind
// must still accept fresh challenges, or email registration can never begin.
func TestFreshChallengesSurviveLocalClockBehindAuthority(t *testing.T) {
	config, _ := accountFixture(t)
	authorityNow := uint64(config.Now().Unix()) + 3
	var expiresAt atomic.Uint64
	expiresAt.Store(authorityNow + 15*60)
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		var status int
		var body any
		switch request.URL.Path {
		case "/v1/accounts":
			status = http.StatusPreconditionRequired
			body = map[string]any{"kind": "verification", "challenge": Challenge{Mode: "email", ExpiresAt: expiresAt.Load()}}
		case "/v1/accounts/email":
			status, body = http.StatusOK, Challenge{ID: "email-challenge", Mode: "email", ExpiresAt: expiresAt.Load()}
		default:
			err := fmt.Errorf("unexpected route %s", request.URL.Path)
			t.Error(err)
			return nil, err
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	service := testService(t, config)
	if err := service.step(t.Context()); err != nil {
		t.Fatalf("registration challenge rejected: %v", err)
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != EmailRequired || snapshot.EmailCodePending {
		t.Fatalf("state=%s reason=%q", snapshot.State, snapshot.Reason)
	}
	if err := service.RequestEmail(t.Context(), "person@example.com"); err != nil {
		t.Fatalf("email challenge rejected: %v", err)
	}
	if snapshot, _ := service.Snapshot(); !snapshot.EmailCodePending {
		t.Fatal("delivered code missing from account snapshot")
	}
	service.mu.Lock()
	proof := service.record.Proof
	service.mu.Unlock()
	if proof == nil || proof.Challenge.ID != "email-challenge" {
		t.Fatalf("email challenge was not saved: %+v", proof)
	}
	expiresAt.Store(uint64(config.Now().Unix()) + 60*60)
	if err := service.RequestEmail(t.Context(), "person@example.com"); err == nil || err.Error() != "invalid email challenge" {
		t.Fatalf("challenge lifetime beyond the authority's: %v", err)
	}
}

func TestVerifyEmailReturnsRegistrationOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		want       error
		superseded bool
	}{
		{name: "registered"},
		{name: "expired at the authority", want: errEmailCodeRejected},
		{name: "email replaced during verification", want: ErrSuperseded, superseded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, session := accountFixture(t)
			now := uint64(config.Now().Unix())
			// A faster authority can reject the code while it is still current locally.
			original := Challenge{ID: "original", Mode: "email", ExpiresAt: now + 1}
			prompt := Challenge{Mode: "email", ExpiresAt: now + uint64(authorityChallengeLifetime.Seconds()) + 2}
			newEmail := Challenge{ID: "new-email", Mode: "email", ExpiresAt: prompt.ExpiresAt}
			config.Store = &MemoryStore{record: Record{SetupSecret: session.Secret, Proof: &Proof{Challenge: original}}}
			config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
				var input struct {
					Secret    string `json:"client_secret"`
					Challenge string `json:"challenge"`
					Solution  string `json:"solution"`
				}
				if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
					return nil, err
				}
				if request.URL.Path != "/v1/accounts" || input.Secret != session.Secret ||
					input.Challenge != original.ID || input.Solution != "local-code" {
					return nil, errors.New("unexpected registration request")
				}
				if test.superseded {
					if _, err := config.Store.Update(request.Context(), func(record Record) (Record, error) {
						record.Proof = &Proof{Challenge: newEmail}
						return record, nil
					}); err != nil {
						return nil, err
					}
				}
				status, body := http.StatusOK, any(session)
				if test.want != nil {
					status, body = http.StatusPreconditionRequired, fault{Kind: "verification", Challenge: prompt}
				}
				data, err := json.Marshal(body)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})
			service := testService(t, config)
			if err := service.VerifyEmail(t.Context(), "local-code"); !errors.Is(err, test.want) {
				t.Fatalf("VerifyEmail = %v, want %v", err, test.want)
			} else if errors.Is(err, errEmailCodeRejected) && CommandErrorCode(err) != "email_code_rejected" {
				t.Fatalf("wrong email verification failure: %v", err)
			}
			record, err := service.load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if test.want == nil {
				if snapshot, _ := service.Snapshot(); snapshot.State != Ready || record.Credential == nil || record.Proof != nil {
					t.Fatalf("verification returned before installing the account: state=%s", snapshot.State)
				}
				return
			}
			wantProof := Proof{Challenge: prompt}
			if test.superseded {
				wantProof.Challenge = newEmail
			}
			if record.Proof == nil || *record.Proof != wantProof || record.Credential != nil {
				t.Fatal("rejected verification left the wrong challenge")
			}
		})
	}
}

func TestVerifyEmailDistinguishesRetryFromReplacement(t *testing.T) {
	for _, test := range []struct {
		name     string
		prompt   func(Challenge) Challenge
		replyLag time.Duration
		want     error
		wantCode string
		pending  bool
	}{
		{
			name: "same challenge", want: errEmailCodeIncorrect,
			wantCode: "email_code_incorrect", pending: true,
		},
		{
			name:     "local deadline crossed while waiting for the same challenge",
			replyLag: time.Second, want: errEmailCodeRejected, wantCode: "email_code_rejected",
		},
		{
			name: "id-less prompt", prompt: func(delivered Challenge) Challenge {
				return Challenge{Mode: emailChallenge, ExpiresAt: delivered.ExpiresAt + 60}
			},
			want: errEmailCodeRejected, wantCode: "email_code_rejected",
		},
		{
			name: "undelivered challenge", prompt: func(delivered Challenge) Challenge {
				delivered.ID = "other"
				return delivered
			},
			want: errInvalidRegistrationChallenge, wantCode: "account_operation_failed", pending: true,
		},
		{
			name: "changed expiry on the same ID", prompt: func(delivered Challenge) Challenge {
				delivered.ExpiresAt++
				return delivered
			},
			want: errInvalidRegistrationChallenge, wantCode: "account_operation_failed", pending: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, session := accountFixture(t)
			now := config.Now()
			config.Now = func() time.Time { return now }
			delivered := Challenge{ID: "delivered", Mode: emailChallenge, ExpiresAt: uint64(now.Unix()) + 1}
			prompt := delivered
			if test.prompt != nil {
				prompt = test.prompt(delivered)
			}
			config.Store = &MemoryStore{record: Record{SetupSecret: session.Secret, Proof: &Proof{Challenge: delivered}}}
			var submitted []string
			config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
				var input struct{ Challenge, Solution string }
				if err := json.NewDecoder(request.Body).Decode(&input); err != nil || request.URL.Path != "/v1/accounts" || input.Challenge != delivered.ID {
					return nil, fmt.Errorf("unexpected registration request %s %+v: %v", request.URL.Path, input, err)
				}
				submitted = append(submitted, input.Solution)
				if input.Solution == "code" {
					return accountReply(t, http.StatusOK, session), nil
				}
				now = now.Add(test.replyLag)
				return accountReply(t, http.StatusPreconditionRequired, fault{Kind: "verification", Challenge: prompt}), nil
			})
			service := testService(t, config)
			if err := service.VerifyEmail(t.Context(), "mistyped"); !errors.Is(err, test.want) || CommandErrorCode(err) != test.wantCode {
				t.Fatalf("mistyped code: %v (%s), want %v (%s)", err, CommandErrorCode(err), test.want, test.wantCode)
			}
			record, err := service.load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			wantProof := Proof{Challenge: prompt}
			if test.want == errInvalidRegistrationChallenge {
				wantProof.Challenge = delivered
			}
			if record.Proof == nil || *record.Proof != wantProof {
				t.Fatalf("saved proof = %+v, want %+v", record.Proof, wantProof)
			}
			if snapshot, _ := service.Snapshot(); snapshot.State != EmailRequired || snapshot.EmailCodePending != test.pending {
				t.Fatalf("after a mistyped code: %+v", snapshot)
			}
			if len(submitted) != 1 {
				t.Fatalf("submissions = %q", submitted)
			}
			if test.want != errEmailCodeIncorrect {
				return
			}
			// An account setup pass must not resubmit the wrong code.
			if err := service.Register(t.Context()); err != nil {
				t.Fatalf("account setup: %v", err)
			}
			if err := service.VerifyEmail(t.Context(), "code"); err != nil {
				t.Fatalf("corrected code: %v", err)
			}
			if snapshot, _ := service.Snapshot(); snapshot.State != Ready || len(submitted) != 2 || submitted[0] != "mistyped" {
				t.Fatalf("corrected code did not register: %s after %q", snapshot.State, submitted)
			}
		})
	}
}

// Pins the accepted expiry window to (now, now + 15 min + ClockSkew] on both
// the registration and the email-request path.
func TestChallengeExpiryBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset uint64
		accept bool
	}{
		{"expires now", 0, false},
		{"one second left", 1, true},
		{"full lifetime", 15 * 60, true},
		{"full lifetime plus skew", 15*60 + serviceauthority.ClockSkew, true},
		{"beyond skew", 15*60 + serviceauthority.ClockSkew + 1, false},
		{"maximum", math.MaxUint64, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, path := range []string{"registration", "email"} {
				config, _ := accountFixture(t)
				expiresAt := uint64(config.Now().Unix()) + test.offset
				if test.offset == math.MaxUint64 {
					expiresAt = math.MaxUint64
				}
				config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, any(Challenge{ID: "email-challenge", Mode: "email", ExpiresAt: expiresAt})
					if request.URL.Path == "/v1/accounts" {
						status = http.StatusPreconditionRequired
						body = map[string]any{"kind": "verification", "challenge": Challenge{Mode: "email", ExpiresAt: expiresAt}}
					}
					data, _ := json.Marshal(body)
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
				})
				service := testService(t, config)
				var err error
				want := "invalid account challenge"
				if path == "registration" {
					err = service.step(t.Context())
				} else {
					err, want = service.RequestEmail(t.Context(), "person@example.com"), "invalid email challenge"
				}
				if test.accept && err != nil {
					t.Fatalf("%s rejected expiry offset %d: %v", path, test.offset, err)
				}
				if !test.accept && (err == nil || err.Error() != want) {
					t.Fatalf("%s expiry offset %d: got %v, want %q", path, test.offset, err, want)
				}
			}
		})
	}
}
