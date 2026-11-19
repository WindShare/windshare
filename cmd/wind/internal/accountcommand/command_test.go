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
	service.action = "retry"
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
func (service *commandService) RevokeCredential(_ context.Context, id [16]byte) error {
	service.action, service.revoked = "revoke", id
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
		{[]string{"password", "alice", "--password-stdin"}, "password", "alice", "a local password"},
		{[]string{"email", "alice@example.test"}, "email", "alice@example.test", ""},
		{[]string{"verify", "local-verification-code"}, "verify", "", "local-verification-code"},
		{[]string{"revoke", base64.RawURLEncoding.EncodeToString(id[:])}, "revoke", "", ""},
	} {
		t.Run(scenario.action, func(t *testing.T) {
			service := &commandService{value: account.Snapshot{State: account.Ready, Account: activeAccount}}
			output := &lines{}
			runner := Runner{Service: service, Input: strings.NewReader("a local password\r\n"), Output: output}
			if code := runner.Run(context.Background(), scenario.args); code != ResultOK {
				t.Fatalf("command failed: %d %s", code, output.String())
			}
			if service.action != scenario.action || service.username != scenario.username || service.secret != scenario.secret {
				t.Fatalf("wrong operation: %+v", service)
			}
			if scenario.action == "password" && service.target != activeAccount {
				t.Fatal("password edit lost its account target")
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
		{"email"}, {"verify"}, {"revoke", "not-an-id"}, {"unknown"},
	} {
		service := &commandService{}
		output := &lines{}
		runner := Runner{Service: service, Input: strings.NewReader("secret"), Output: output}
		if code := runner.Run(context.Background(), args); code != ResultFailure || service.action != "" {
			t.Fatalf("invalid command mutated account: %v %d %s", args, code, service.action)
		}
	}
}

func TestAccountRetryUsesAttemptResult(t *testing.T) {
	denied := errors.New("account_blocked")
	for _, scenario := range []struct {
		name    string
		outcome account.RefreshOutcome
		err     error
		code    int
		hint    string
	}{
		{name: "refreshed", outcome: account.RefreshSucceeded, code: ResultOK},
		{name: "denied", err: denied, code: ResultFailure, hint: denied.Error()},
		{name: "timed out", err: context.DeadlineExceeded, code: ResultFailure},
		{name: "register", outcome: account.RefreshNeedsRegistration, code: ResultFailure, hint: "wind account register"},
		{name: "verify email", outcome: account.RefreshNeedsEmail, code: ResultFailure, hint: "wind account verify"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := &commandService{
				value: account.Snapshot{State: account.Ready},
				refresh: func(context.Context) (account.RefreshOutcome, error) {
					return scenario.outcome, scenario.err
				},
			}
			output := &lines{}
			code := (Runner{Service: service, Output: output}).Run(t.Context(), []string{"retry"})
			if code != scenario.code || service.action != "retry" {
				t.Fatalf("retry returned %d without the expected attempt result: %s", code, output)
			}
			if scenario.hint != "" && !strings.Contains(output.String(), scenario.hint) {
				t.Fatalf("retry omitted recovery guidance: %s", output)
			}
		})
	}
}

func TestAccountRetryWaitsForAttemptDespiteCachedReadiness(t *testing.T) {
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
		done := make(chan int, 1)
		go func() {
			done <- (Runner{Service: service, Output: &lines{}}).Run(ctx, []string{"retry"})
		}()
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Fatal("retry never started the refresh attempt")
		}
		select {
		case code := <-done:
			t.Fatalf("retry returned %d while the attempt was still in flight", code)
		default:
		}
		cancel()
		if code := <-done; code != ResultFailure {
			t.Fatalf("canceled retry returned %d", code)
		}
	})
}

func TestAccountStatusAndErrors(t *testing.T) {
	output := &lines{}
	if code := (Runner{Output: output}).Run(context.Background(), nil); code != ResultFailure {
		t.Fatal("unconfigured service accepted")
	}
	service := &commandService{value: account.Snapshot{State: account.RefreshRequired, Account: "account", Credential: "credential"}}
	var encoded bytes.Buffer
	runner := Runner{Service: service, Output: output, JSONOutput: &encoded}
	if code := runner.Run(context.Background(), []string{"status", "--json"}); code != ResultOK {
		t.Fatal(code)
	}
	var snapshot account.Snapshot
	if err := json.Unmarshal(encoded.Bytes(), &snapshot); err != nil || snapshot.Account != service.value.Account {
		t.Fatalf("invalid account status: %v", err)
	}
	if code := runner.Run(context.Background(), nil); code != ResultOK || service.action != "" {
		t.Fatal("reading status performed an account operation")
	}
	service.failure = errors.New("request unavailable")
	if code := runner.Run(context.Background(), []string{"email", "alice@example.test"}); code != ResultFailure {
		t.Fatal("authority failure lost")
	}
}
