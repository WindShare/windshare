package accountcommand

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/engine/account"
)

type commandService struct {
	value    account.Snapshot
	action   string
	username string
	target   string
	secret   string
	revoked  [16]byte
	failure  error
	refresh  func(context.Context) (account.RefreshOutcome, error)
}

func (service *commandService) Snapshot() (account.Snapshot, <-chan struct{}) {
	return service.value, nil
}
func (service *commandService) RefreshOnce(ctx context.Context) (account.RefreshOutcome, error) {
	service.action = "refresh"
	return service.refresh(ctx)
}
func (service *commandService) Register(context.Context) error {
	service.action = "register"
	return service.failure
}
func (service *commandService) Login(_ context.Context, username, password string) error {
	service.action, service.username, service.secret = "login", username, password
	return service.failure
}
func (service *commandService) SetPassword(_ context.Context, accountID, username, password string) error {
	service.action, service.target, service.username, service.secret = "password", accountID, username, password
	return service.failure
}
func (service *commandService) RequestEmail(_ context.Context, email string) error {
	service.action, service.username = "email", email
	return service.failure
}
func (service *commandService) VerifyEmail(_ context.Context, code string) error {
	service.action, service.secret = "verify", code
	return service.failure
}
func (service *commandService) RevokeCredential(_ context.Context, accountID string, id [16]byte) error {
	service.action, service.target, service.revoked = "revoke", accountID, id
	return service.failure
}

type lines struct{ bytes.Buffer }

func (output *lines) WriteLines(values ...string) {
	for _, value := range values {
		output.WriteString(value)
		output.WriteByte('\n')
	}
}

func TestAccountCommandsDispatchSecretsWithoutEcho(t *testing.T) {
	const activeAccount = "active-account"
	id := [16]byte{17}
	for _, scenario := range []struct {
		args                     []string
		action, username, secret string
	}{
		{[]string{"register"}, "register", "", ""},
		{[]string{"login", "alice", "--password-stdin"}, "login", "alice", "a local password"},
		{[]string{"login", "--password-stdin", "alice"}, "login", "alice", "a local password"},
		{[]string{"password", "alice", "--password-stdin"}, "password", "alice", "a local password"},
		{[]string{"password", "--password-stdin", "alice"}, "password", "alice", "a local password"},
		{[]string{"email", "alice@example.test"}, "email", "alice@example.test", ""},
		{[]string{"verify", "local-verification-code"}, "verify", "", "local-verification-code"},
		{[]string{"revoke", base64.RawURLEncoding.EncodeToString(id[:])}, "revoke", "", ""},
	} {
		t.Run(scenario.action, func(t *testing.T) {
			service := &commandService{value: account.Snapshot{State: account.Ready, Account: activeAccount}}
			output := &lines{}
			runner := Runner{Service: service, Input: strings.NewReader("a local password\r\n"), Output: output}
			if code := runner.Run(context.Background(), scenario.args); code != command.ExitOK {
				t.Fatalf("command failed: %d %s", code, output.String())
			}
			if service.action != scenario.action || service.username != scenario.username || service.secret != scenario.secret {
				t.Fatalf("wrong operation: %+v", service)
			}
			if (scenario.action == "password" || scenario.action == "revoke") && service.target != activeAccount {
				t.Fatalf("%s lost its account target", scenario.action)
			}
			if scenario.action == "revoke" && service.revoked != id {
				t.Fatal("credential identity changed")
			}
			if strings.Contains(output.String(), "a local password") || strings.Contains(output.String(), "local-verification-code") {
				t.Fatal("secret echoed to output")
			}
		})
	}
}

func TestAccountCommandsRejectInvalidInputBeforeMutation(t *testing.T) {
	for _, args := range [][]string{
		{"status", "unexpected"}, {"login", "alice"}, {"login"}, {"password", "alice", "--unknown"},
		{"email"}, {"verify"}, {"revoke", "not-an-id"}, {"retry"}, {"unknown"},
	} {
		service := &commandService{}
		output := &lines{}
		runner := Runner{Service: service, Input: strings.NewReader("secret"), Output: output}
		if code := runner.Run(context.Background(), args); code != command.ExitUsage || service.action != "" {
			t.Fatalf("invalid command returned %d or mutated account: %v %s", code, args, service.action)
		}
	}
	service := &commandService{value: account.Snapshot{State: account.Pending}}
	output := &lines{}
	id := [16]byte{19}
	if code := (Runner{Service: service, Output: output}).Run(context.Background(), []string{"revoke", base64.RawURLEncoding.EncodeToString(id[:])}); code != command.ExitFailure || service.action != "" || !strings.Contains(output.String(), "account setup is still pending") {
		t.Fatalf("pending revoke returned %d action=%q output=%q", code, service.action, output.String())
	}
}

func TestAccountHelpSucceedsWithoutConfiguredService(t *testing.T) {
	for _, args := range [][]string{
		{"help"}, {"-h"}, {"-help"}, {"--help"},
		{"status", "--help"}, {"refresh", "-h"}, {"login", "--help"},
		{"login", "alice", "--help"}, {"password", "alice", "-h"}, {"email", "alice@example.test", "--help"},
	} {
		output := &lines{}
		if code := (Runner{Output: output}).Run(context.Background(), args); code != command.ExitOK || !strings.Contains(output.String(), "wind account status") {
			t.Fatalf("help %v returned %d output=%q", args, code, output.String())
		}
	}
}

func TestAccountRefreshUsesAttemptResult(t *testing.T) {
	denied := errors.New("account_blocked")
	for _, scenario := range []struct {
		name    string
		outcome account.RefreshOutcome
		err     error
		code    command.ExitCode
		hint    string
	}{
		{name: "refreshed", outcome: account.RefreshSucceeded, code: command.ExitOK},
		{name: "denied", err: denied, code: command.ExitFailure, hint: denied.Error()},
		{name: "timed out", err: context.DeadlineExceeded, code: command.ExitFailure},
		{name: "register", outcome: account.RefreshNeedsRegistration, code: command.ExitFailure, hint: "wind account register"},
		{name: "verify email", outcome: account.RefreshNeedsEmail, code: command.ExitFailure, hint: "wind account verify"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := &commandService{
				value: account.Snapshot{State: account.Ready},
				refresh: func(context.Context) (account.RefreshOutcome, error) {
					return scenario.outcome, scenario.err
				},
			}
			output := &lines{}
			code := (Runner{Service: service, Output: output}).Run(t.Context(), []string{"refresh"})
			if code != scenario.code || service.action != "refresh" {
				t.Fatalf("refresh returned %d without the expected attempt result: %s", code, output)
			}
			if scenario.hint != "" && !strings.Contains(output.String(), scenario.hint) {
				t.Fatalf("refresh omitted recovery guidance: %s", output)
			}
		})
	}
}

func TestAccountRefreshWaitsForAttemptDespiteCachedReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		service := &commandService{
			value: account.Snapshot{State: account.Ready},
			refresh: func(ctx context.Context) (account.RefreshOutcome, error) {
				close(started)
				<-ctx.Done()
				return "", ctx.Err()
			},
		}
		done := make(chan command.ExitCode, 1)
		go func() {
			done <- (Runner{Service: service, Output: &lines{}}).Run(ctx, []string{"refresh"})
		}()
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Fatal("refresh never started the refresh attempt")
		}
		select {
		case code := <-done:
			t.Fatalf("refresh returned %d while the attempt was still in flight", code)
		default:
		}
		cancel()
		if code := <-done; code != command.ExitFailure {
			t.Fatalf("canceled refresh returned %d", code)
		}
	})
}

func TestAccountStatusAndErrors(t *testing.T) {
	output := &lines{}
	if code := (Runner{Output: output}).Run(context.Background(), nil); code != command.ExitFailure {
		t.Fatal("unconfigured service accepted")
	}
	service := &commandService{value: account.Snapshot{
		State:        account.RefreshRequired,
		Account:      "account",
		Credential:   "credential",
		Username:     "alice",
		Profile:      account.Profile{Nickname: account.Nickname{Value: "Alice"}, Email: "alice@example.test"},
		PendingEmail: account.PendingEmail{Address: "next@example.test"},
	}}
	var encoded bytes.Buffer
	runner := Runner{Service: service, Output: output, JSONOutput: &encoded}
	if code := runner.Run(context.Background(), []string{"status", "--json"}); code != command.ExitOK {
		t.Fatal(code)
	}
	var snapshot account.Snapshot
	if err := json.Unmarshal(encoded.Bytes(), &snapshot); err != nil || snapshot.Account != service.value.Account {
		t.Fatalf("invalid account status: %v", err)
	}
	output.Reset()
	if code := runner.Run(context.Background(), nil); code != command.ExitOK || service.action != "" {
		t.Fatal("reading status performed an account operation")
	}
	for _, want := range []string{
		"wind account refresh",
		"Nickname: Alice",
		"Username: alice",
		"Email: alice@example.test",
		"Pending email: next@example.test",
		"Account: account",
		"Credential: credential",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, output.String())
		}
	}
	service.failure = errors.New("request unavailable")
	if code := runner.Run(context.Background(), []string{"email", "alice@example.test"}); code != command.ExitFailure {
		t.Fatal("authority failure lost")
	}
	output.Reset()
	storageService := &commandService{
		value:   account.Snapshot{State: account.StorageError, Reason: account.ErrInvalidState.Error()},
		failure: account.ErrInvalidState,
	}
	storageRunner := Runner{Service: storageService, Output: output}
	if code := storageRunner.Run(context.Background(), []string{"status"}); code != command.ExitOK || !strings.Contains(output.String(), "Local account file needs repair") {
		t.Fatalf("storage status returned %d output=%q", code, output.String())
	}
	output.Reset()
	if code := storageRunner.Run(context.Background(), []string{"register"}); code != command.ExitFailure || !strings.Contains(output.String(), "local account file is unreadable or invalid") {
		t.Fatalf("storage register returned %d output=%q", code, output.String())
	}
}
