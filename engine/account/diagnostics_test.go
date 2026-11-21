package account

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

func captureSlogForTest(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func drainDiagnostics(stream *diagnostics.Stream) []diagnostics.Record {
	stream.Close()
	var records []diagnostics.Record
	for record := range stream.Records() {
		records = append(records, record)
	}
	return records
}

func TestAccountDiagnosticsCaptureLifecycleAndRenewalWithoutSlog(t *testing.T) {
	slogOutput := captureSlogForTest(t)
	stream, err := diagnostics.NewStream(64, nil)
	if err != nil {
		t.Fatal(err)
	}
	config, issued := accountFixture(t)
	config.Diagnostics = stream.Sink()

	calls := 0
	var secret string
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		reply := issued
		if calls == 1 {
			saved, err := config.Store.Load(request.Context())
			if err != nil {
				t.Fatal(err)
			}
			secret = saved.SetupSecret
		}
		reply.Secret = secret
		return accountReply(t, http.StatusOK, reply), nil
	})

	service := testService(t, config)
	if outcome, err := service.RefreshOnce(t.Context()); err != nil || outcome != RefreshSucceeded {
		t.Fatalf("initial refresh = %s, %v", outcome, err)
	}
	// Ready -> Ready renewal must still record ticket issuance for trace visibility.
	if outcome, err := service.RefreshOnce(t.Context()); err != nil || outcome != RefreshSucceeded {
		t.Fatalf("steady renewal = %s, %v", outcome, err)
	}
	service.Close()

	if slogOutput.Len() != 0 {
		t.Fatalf("account service bypassed diagnostics sink via slog: %s", slogOutput.String())
	}

	records := drainDiagnostics(stream)
	wantAccount := base64.RawURLEncoding.EncodeToString(issued.Account[:])
	wantCredential := base64.RawURLEncoding.EncodeToString(issued.Credential[:])
	var stateChanges, ticketIssues, attempts int
	for _, record := range records {
		if strings.Contains(record.Payload().Text(), secret) {
			t.Fatal("diagnostic payload leaked setup/credential secret")
		}
		switch record.Name() {
		case "account_state_changed":
			if record.Payload().Get("from").Text() == string(Pending) &&
				record.Payload().Get("to").Text() == string(Ready) &&
				record.Payload().Get("account").Text() == wantAccount &&
				record.Payload().Get("credential").Text() == wantCredential {
				stateChanges++
			}
		case "account_ticket_issued":
			if record.Payload().Get("account").Text() == wantAccount &&
				record.Payload().Get("credential").Text() == wantCredential &&
				record.Payload().Get("entitlement_version").Uint() == issued.Entitlement.Version &&
				record.Payload().Get("expires_at").Text() != "" &&
				record.Payload().Get("valid_until").Text() != "" {
				ticketIssues++
			}
		case "account_refresh_attempt_completed":
			if record.Payload().Get("outcome").Text() == string(RefreshSucceeded) {
				attempts++
			}
		}
	}
	if stateChanges != 1 || ticketIssues != 2 || attempts != 2 {
		t.Fatalf("diagnostics counts: stateChanges=%d ticketIssues=%d attempts=%d (total=%d)",
			stateChanges, ticketIssues, attempts, len(records))
	}
}

func TestAccountDiagnosticsCaptureRegistrationFallbackAndStorageErrorWithoutSlog(t *testing.T) {
	slogOutput := captureSlogForTest(t)
	stream, err := diagnostics.NewStream(64, nil)
	if err != nil {
		t.Fatal(err)
	}
	config, issued := accountFixture(t)
	config.Diagnostics = stream.Sink()
	challenge := Challenge{
		ID:        "work-1",
		Mode:      workChallenge,
		Bits:      4,
		ExpiresAt: uint64(config.Now().Unix()) + 60,
	}
	calls := 0
	config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return accountReply(t, http.StatusPreconditionRequired, fault{Kind: "verification", Challenge: challenge}), nil
		case 2:
			return nil, errors.New("network lost")
		default:
			return accountReply(t, http.StatusServiceUnavailable, fault{Kind: "unavailable", Reason: "busy"}), nil
		}
	})

	service := testService(t, config)
	if err := service.Register(t.Context()); err == nil {
		t.Fatal("expected registration network failure")
	}

	// Simulate an unavailable logged-in account falling back to an anonymous-capable node.
	service.mu.Lock()
	service.record = cachedRecord(issued)
	service.record.Ticket = ""
	service.state = Unavailable
	service.retryAt = config.Now().Add(time.Minute)
	service.nodes.put("https://node.example", &nodeState{
		key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour),
	})
	service.mu.Unlock()
	share := &ShareAuthorization{service: service}
	service.mu.Lock()
	share.credential = service.retainCredentialLocked(service.record)
	service.mu.Unlock()
	source := &Source{share: share, address: "https://node.example"}
	auth := source.ReserveRegistrationAuthorization()
	if auth.State != serviceauthority.AdmissionReady || auth.Bearer != "" {
		t.Fatalf("expected anonymous fallback, got %+v", auth)
	}
	auth.Reservation.Finish(serviceauthority.RegistrationMayHaveBeenSent)
	share.Close()
	service.Close()

	if slogOutput.Len() != 0 {
		t.Fatalf("account registration/fallback bypassed diagnostics sink via slog: %s", slogOutput.String())
	}

	seen := map[string]bool{}
	for _, record := range drainDiagnostics(stream) {
		seen[record.Name()] = true
	}
	for _, want := range []string{
		"account_registration_challenge_received",
		"account_registration_verifying",
		"account_registration_submitting",
		"account_registration_failed",
		"account_share_anonymous_fallback",
	} {
		if !seen[want] {
			t.Fatalf("missing expected diagnostic event %q; saw %v", want, seen)
		}
	}
}
