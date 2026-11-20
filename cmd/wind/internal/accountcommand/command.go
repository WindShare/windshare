// Package accountcommand presents the engine's shared official-service account.
package accountcommand

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/commandinput"
	"github.com/windshare/windshare/cmd/wind/internal/commandmeta"
	"github.com/windshare/windshare/engine/account"
	"golang.org/x/term"
)

const (
	ResultOK      = 0
	ResultFailure = 1
	ResultUsage   = 2
)

const Usage = "Usage:\n" +
	"  " + commandmeta.Name + " account status [--json] | register | refresh\n" +
	"  " + commandmeta.Name + " account login <username> | password <username> [--password-stdin]\n" +
	"  " + commandmeta.Name + " account email <address> | verify <code> | revoke <credential>"

type usageError struct{ message string }

func (err usageError) Error() string { return err.message }

func IsHelp(args []string) bool {
	if len(args) == 1 {
		return args[0] == "help" || isHelpFlag(args[0])
	}
	if len(args) == 2 && isHelpFlag(args[1]) {
		switch args[0] {
		case "status", "register", "refresh", "login", "password", "email", "verify", "revoke":
			return true
		}
	}
	return false
}

func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

type LineOutput interface{ WriteLines(...string) }

type Service interface {
	Snapshot() (account.Snapshot, <-chan struct{})
	RefreshOnce(context.Context) (account.RefreshOutcome, error)
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

type parsedCommand struct {
	action        string
	jsonOutput    bool
	value         string
	passwordStdin bool
	credential    [16]byte
}

func parseCommand(args []string) (parsedCommand, error) {
	if len(args) == 0 {
		return parsedCommand{action: "status"}, nil
	}
	switch args[0] {
	case "status":
		if len(args) == 1 {
			return parsedCommand{action: "status"}, nil
		}
		if len(args) == 2 && args[1] == "--json" {
			return parsedCommand{action: "status", jsonOutput: true}, nil
		}
	case "register", "refresh":
		if len(args) == 1 {
			return parsedCommand{action: args[0]}, nil
		}
	case "login", "password":
		if len(args) == 2 || (len(args) == 3 && args[2] == "--password-stdin") {
			return parsedCommand{action: args[0], value: args[1], passwordStdin: len(args) == 3}, nil
		}
	case "email", "verify":
		if len(args) == 2 {
			return parsedCommand{action: args[0], value: args[1]}, nil
		}
	case "revoke":
		if len(args) == 2 {
			raw, err := base64.RawURLEncoding.Strict().DecodeString(args[1])
			if err != nil || len(raw) != 16 {
				return parsedCommand{}, usageError{message: "Account: invalid credential ID"}
			}
			return parsedCommand{action: "revoke", credential: [16]byte(raw)}, nil
		}
	}
	return parsedCommand{}, usageError{message: Usage}
}

func (runner Runner) Run(ctx context.Context, args []string) int {
	if IsHelp(args) {
		runner.Output.WriteLines(Usage)
		return ResultOK
	}
	command, err := parseCommand(args)
	if err != nil {
		runner.Output.WriteLines(err.Error())
		return ResultUsage
	}
	if runner.Service == nil {
		runner.Output.WriteLines(account.ErrNotConfigured.Error())
		return ResultFailure
	}
	if command.action == "status" && command.jsonOutput {
		snapshot, _ := runner.Service.Snapshot()
		if err := json.NewEncoder(runner.JSONOutput).Encode(snapshot); err != nil {
			return ResultFailure
		}
		return ResultOK
	}
	if err := runner.execute(ctx, command); err != nil {
		if usage, ok := errors.AsType[usageError](err); ok {
			runner.Output.WriteLines(usage.message)
			return ResultUsage
		}
		runner.Output.WriteLines("Account: " + err.Error())
		return ResultFailure
	}
	snapshot, _ := runner.Service.Snapshot()
	if command.action == "verify" {
		runner.Output.WriteLines("Account setup complete.")
	} else {
		runner.Output.WriteLines(Describe(snapshot))
	}
	if identity := IdentityLines(snapshot); len(identity) > 0 {
		runner.Output.WriteLines(identity...)
	}
	return ResultOK
}
func (runner Runner) execute(ctx context.Context, command parsedCommand) error {
	switch command.action {
	case "register":
		return runner.register(ctx)
	case "status":
		return nil
	case "refresh":
		return runner.refresh(ctx)
	case "login":
		password, err := runner.password(ctx, command.passwordStdin)
		if err != nil {
			return err
		}
		return runner.Service.Login(ctx, command.value, password)
	case "password":
		return runner.setPassword(ctx, command.value, command.passwordStdin)
	case "email":
		return runner.Service.RequestEmail(ctx, command.value)
	case "verify":
		return runner.Service.VerifyEmail(ctx, command.value)
	case "revoke":
		return runner.Service.RevokeCredential(ctx, command.credential)
	default:
		return usageError{message: Usage}
	}
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

func (runner Runner) setPassword(ctx context.Context, username string, stdin bool) error {
	// Bind the update to the current account before password entry can block.
	snapshot, _ := runner.Service.Snapshot()
	if snapshot.Account == "" {
		return errors.New("account setup is still pending")
	}
	password, err := runner.password(ctx, stdin)
	if err != nil {
		return err
	}
	return runner.Service.SetPassword(ctx, snapshot.Account, username, password)
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
		return "", usageError{message: "Account: use --password-stdin when input is redirected"}
	}
	runner.Output.WriteLines("Password:")
	secret, err := commandinput.Read(ctx, func() ([]byte, error) { return term.ReadPassword(int(input.Fd())) })
	defer clear(secret)
	if err != nil {
		return "", err
	}
	return string(secret), nil
}
func (runner Runner) refresh(ctx context.Context) error {
	runner.Output.WriteLines(Describe(account.Snapshot{State: account.Refreshing}))
	outcome, err := runner.Service.RefreshOnce(ctx)
	if err != nil {
		return err
	}
	switch outcome {
	case account.RefreshSucceeded:
		return nil
	case account.RefreshNeedsRegistration:
		return errors.New(Describe(account.Snapshot{State: account.RegistrationRequired}))
	case account.RefreshNeedsEmail:
		return errors.New(Describe(account.Snapshot{State: account.EmailRequired}))
	default:
		return errors.New("account refresh did not complete")
	}
}
