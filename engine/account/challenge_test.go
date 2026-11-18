package account

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
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
			t.Fatalf("unexpected route %s", request.URL.Path)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	service := testService(t, config)
	if err := service.step(t.Context()); err != nil {
		t.Fatalf("registration challenge rejected: %v", err)
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != EmailRequired {
		t.Fatalf("state=%s reason=%q", snapshot.State, snapshot.Reason)
	}
	if err := service.RequestEmail(t.Context(), "person@example.com"); err != nil {
		t.Fatalf("email challenge rejected: %v", err)
	}
	service.mu.Lock()
	proof := service.record.Proof
	service.mu.Unlock()
	if proof == nil || proof.Challenge.ID != "email-challenge" {
		t.Fatalf("email challenge was not saved: %+v", proof)
	}
	expiresAt.Store(uint64(config.Now().Unix()) + 60*60)
	if err := service.RequestEmail(t.Context(), "person@example.com"); err == nil {
		t.Fatal("accepted a challenge lifetime beyond the authority's")
	}
}
