package accountcommand

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/account"
)

type commandService struct {
	value    account.Snapshot
	changed  chan struct{}
	action   string
	username string
	secret   string
	revoked  [16]byte
	failure  error
	onRetry  func()
}

func (service *commandService) Snapshot() (account.Snapshot, <-chan struct{}) {
	return service.value, service.changed
}
func (service *commandService) Retry() {
	service.action = "retry"
	if service.onRetry != nil {
		service.onRetry()
	}
}
func (service *commandService) Register(context.Context) error {
	service.action = "register"
	return service.failure
}
func (service *commandService) Login(_ context.Context, username, password string) error {
	service.action, service.username, service.secret = "login", username, password
	return service.failure
}
func (service *commandService) SetPassword(_ context.Context, username, password string) error {
	service.action, service.username, service.secret = "password", username, password
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
			service := &commandService{value: account.Snapshot{State: account.Ready}}
			output := &lines{}
			runner := Runner{Service: service, Input: strings.NewReader("a local password\r\n"), Output: output}
			if code := runner.Run(context.Background(), scenario.args); code != ResultOK {
				t.Fatalf("command failed: %d %s", code, output.String())
			}
			if service.action != scenario.action || service.username != scenario.username || service.secret != scenario.secret {
				t.Fatalf("wrong operation: %+v", service)
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

func TestAccountRetryHonorsCancellationAndExplicitDenial(t *testing.T) {
	for _, state := range []account.State{account.Pending, account.Registering, account.Refreshing, account.Unavailable, account.Denied} {
		t.Run(string(state), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := &commandService{value: account.Snapshot{State: state, Active: true, Reason: "account_blocked"}, changed: make(chan struct{})}
			if state != account.Denied {
				service.onRetry = cancel
			}
			output := &lines{}
			if code := (Runner{Service: service, Output: output}).Run(ctx, []string{"retry"}); code != ResultFailure {
				t.Fatal("retry discarded denial/cancellation")
			}
			if service.action != "retry" {
				t.Fatal("setup was not requested")
			}
		})
	}
	for _, state := range []account.State{account.Ready, account.EmailRequired, account.RegistrationRequired} {
		service := &commandService{value: account.Snapshot{State: state}}
		if code := (Runner{Service: service, Output: &lines{}}).Run(context.Background(), []string{"retry"}); code != ResultOK {
			t.Fatal("ready or interactive verification remained blocked")
		}
	}
}

func TestAccountStatusAndErrors(t *testing.T) {
	output := &lines{}
	if code := (Runner{Output: output}).Run(context.Background(), nil); code != ResultFailure {
		t.Fatal("unconfigured service accepted")
	}
	service := &commandService{value: account.Snapshot{State: account.Pending, Account: "account", Credential: "credential"}}
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
