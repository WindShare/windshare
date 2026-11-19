// Package accountcommand presents the engine's shared official-service account.
package accountcommand

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/commandinput"
	"github.com/windshare/windshare/engine/account"
	"golang.org/x/term"
)

const (
	ResultOK      = 0
	ResultFailure = 1
	ResultUsage   = 2
)

type LineOutput interface{ WriteLines(...string) }

type Service interface {
	Snapshot() (account.Snapshot, <-chan struct{})
	Retry()
	Register(context.Context) error
	Login(context.Context, string, string) error
	SetPassword(context.Context, string, string, string) error
	RequestEmail(context.Context, string) error
	VerifyEmail(context.Context, string) error
	RevokeCredential(context.Context, [16]byte) error
}

type Runner struct {
	Service    Service
	Input      io.Reader
	Output     LineOutput
	JSONOutput io.Writer
}

func (runner Runner) Run(ctx context.Context, args []string) int {
	if runner.Service == nil {
		runner.Output.WriteLines(account.ErrNotConfigured.Error())
		return ResultFailure
	}
	if len(args) == 0 {
		args = []string{"status"}
	}
	if len(args) == 2 && args[0] == "status" && args[1] == "--json" {
		snapshot, _ := runner.Service.Snapshot()
		if err := json.NewEncoder(runner.JSONOutput).Encode(snapshot); err != nil {
			return ResultFailure
		}
		return ResultOK
	}
	if err := runner.execute(ctx, args); err != nil {
		runner.Output.WriteLines("Account: " + err.Error())
		return ResultFailure
	}
	snapshot, _ := runner.Service.Snapshot()
	if args[0] == "verify" {
		runner.Output.WriteLines("Account setup complete.")
	} else {
		runner.Output.WriteLines(Describe(snapshot))
	}
	if snapshot.Account != "" {
		runner.Output.WriteLines("Account: "+snapshot.Account, "Credential: "+snapshot.Credential)
	}
	return ResultOK
}
func (runner Runner) execute(ctx context.Context, args []string) error {
	switch args[0] {
	case "register":
		if len(args) == 1 {
			return runner.register(ctx)
		}
	case "status":
		if len(args) == 1 {
			return nil
		}
	case "retry":
		if len(args) == 1 {
			runner.Service.Retry()
			return runner.wait(ctx)
		}
	case "login", "password":
		if len(args) == 2 || len(args) == 3 && args[2] == "--password-stdin" {
			var accountID string
			if args[0] == "password" {
				// Password entry can block while another process switches accounts.
				snapshot, _ := runner.Service.Snapshot()
				if snapshot.Account == "" {
					return errors.New("account setup is still pending")
				}
				accountID = snapshot.Account
			}
			password, err := runner.password(ctx, len(args) == 3)
			if err != nil {
				return err
			}
			if args[0] == "login" {
				return runner.Service.Login(ctx, args[1], password)
			}
			return runner.Service.SetPassword(ctx, accountID, args[1], password)
		}
	case "email":
		if len(args) == 2 {
			return runner.Service.RequestEmail(ctx, args[1])
		}
	case "verify":
		if len(args) == 2 {
			return runner.Service.VerifyEmail(ctx, args[1])
		}
	case "revoke":
		if len(args) == 2 {
			raw, err := base64.RawURLEncoding.Strict().DecodeString(args[1])
			if err != nil || len(raw) != 16 {
				return errors.New("invalid credential ID")
			}
			return runner.Service.RevokeCredential(ctx, [16]byte(raw))
		}
	}
	return errors.New("use wind account status [--json], register, retry, login <username>, password <username>, email <address>, verify <code>, or revoke <credential>")
}

func (runner Runner) register(ctx context.Context) error {
	runner.Output.WriteLines("Creating your account…")
	done := make(chan error, 1)
	go func() { done <- runner.Service.Register(ctx) }()
	// A redirected terminal should get a few useful milestones, even on slow devices.
	const milestone = 100 // ten percent, in thousandths
	var previous uint16
	for {
		value, changed := runner.Service.Snapshot()
		if step := value.RegistrationPermille / milestone; value.State == account.Registering && step > previous {
			runner.Output.WriteLines(Describe(value))
			previous = step
		}
		select {
		case err := <-done:
			if err == nil || ctx.Err() != nil {
				return err
			}
			switch account.CommandErrorCode(err) {
			case "rate_limited":
				return errors.New("too many attempts, please try again later")
			case "account_service_unreachable":
				return errors.New("could not reach the official relay service, please try again")
			case "account_changed":
				return errors.New("the account changed, run wind account status to check it")
			case "account_denied":
				return errors.New("this account was denied access to the official relay service")
			default:
				return errors.New("could not create your account, please try again")
			}
		case <-changed:
		}
	}
}

func (runner Runner) password(ctx context.Context, stdin bool) (string, error) {
	if stdin {
		text, err := commandinput.ReadLine(ctx, bufio.NewReader(io.LimitReader(runner.Input, 258)))
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(text, "\r\n"), nil
	}
	input, ok := runner.Input.(*os.File)
	if !ok || !term.IsTerminal(int(input.Fd())) {
		return "", errors.New("use --password-stdin when input is redirected")
	}
	runner.Output.WriteLines("Password:")
	secret, err := commandinput.Read(ctx, func() ([]byte, error) { return term.ReadPassword(int(input.Fd())) })
	defer clear(secret)
	if err != nil {
		return "", err
	}
	return string(secret), nil
}
func (runner Runner) wait(ctx context.Context) error {
	var previous string
	for {
		value, changed := runner.Service.Snapshot()
		message := Describe(value)
		if value.State == account.Ready {
			return nil
		}
		if value.State == account.EmailRequired || value.State == account.RegistrationRequired {
			return nil
		}
		if value.State == account.Denied {
			return fmt.Errorf("access denied: %s", value.Reason)
		}
		if message != previous {
			runner.Output.WriteLines(message)
			previous = message
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
