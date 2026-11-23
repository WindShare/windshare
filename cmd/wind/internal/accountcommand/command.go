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

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/cmd/wind/internal/commandinput"
	"github.com/windshare/windshare/engine/account"
	"golang.org/x/term"
)

const Usage = "Usage:\n" +
	"  " + command.Name + " account status [--json] | register | refresh\n" +
	"  " + command.Name + " account login <username> | password <username> [--password-stdin]\n" +
	"  " + command.Name + " account email <address> | verify <code> | revoke <credential>"

type usageError struct{ message string }

func (err usageError) Error() string { return err.message }

type Service interface {
	Snapshot() (account.Snapshot, <-chan struct{})
	RefreshOnce(context.Context) (account.RefreshOutcome, error)
	Register(context.Context) error
	Login(context.Context, string, string) error
	SetPassword(context.Context, string, string, string) error
	RequestEmail(context.Context, string) error
	VerifyEmail(context.Context, string) error
	RevokeCredential(context.Context, string, [16]byte) error
}

type Runner struct {
	Service    Service
	Input      io.Reader
	Output     command.LineOutput
	JSONOutput io.Writer
}

// Request is a validated account subcommand ready to execute against a Service.
type Request struct {
	action        string
	jsonOutput    bool
	value         string
	passwordStdin bool
	credential    [16]byte
}

// Parse validates account subcommand arguments without requiring an initialized
// engine or account service. When ready is false, code is the terminal exit status.
func Parse(args []string, output command.LineOutput) (Request, command.ExitCode, bool) {
	if len(args) == 0 {
		return Request{action: "status"}, command.ExitOK, true
	}
	if command.IsHelpToken(args[0]) {
		command.WriteLines(output, Usage)
		return Request{}, command.ExitOK, false
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "status":
		var req Request
		req.action = action
		flags := command.NewFlagSet("account status")
		flags.BoolVar(&req.jsonOutput, "json", false, "write account status as JSON")
		if code, ok := projectAccountFlagParse(output, "account status", command.ParseNoPositionals(flags, rest)); !ok {
			return Request{}, code, false
		}
		return req, command.ExitOK, true
	case "register", "refresh":
		flags := command.NewFlagSet("account " + action)
		if code, ok := projectAccountFlagParse(output, "account "+action, command.ParseNoPositionals(flags, rest)); !ok {
			return Request{}, code, false
		}
		return Request{action: action}, command.ExitOK, true
	case "login", "password":
		var req Request
		req.action = action
		flags := command.NewFlagSet("account " + action)
		flags.BoolVar(&req.passwordStdin, "password-stdin", false, "read password from standard input")
		positionals, outcome := command.ParseInterleaved(flags, rest)
		if code, ok := projectAccountFlagParse(output, "account "+action, outcome); !ok {
			return Request{}, code, false
		}
		if len(positionals) != 1 || positionals[0] == "" {
			command.WriteLines(output, Usage)
			return Request{}, command.ExitUsage, false
		}
		req.value = positionals[0]
		return req, command.ExitOK, true
	case "email", "verify":
		flags := command.NewFlagSet("account " + action)
		positionals, outcome := command.ParseInterleaved(flags, rest)
		if code, ok := projectAccountFlagParse(output, "account "+action, outcome); !ok {
			return Request{}, code, false
		}
		if len(positionals) != 1 || positionals[0] == "" {
			command.WriteLines(output, Usage)
			return Request{}, command.ExitUsage, false
		}
		return Request{action: action, value: positionals[0]}, command.ExitOK, true
	case "revoke":
		positionals, outcome := parseRevokePositionals(rest)
		if code, ok := projectAccountFlagParse(output, "account revoke", outcome); !ok {
			return Request{}, code, false
		}
		if len(positionals) != 1 || positionals[0] == "" {
			command.WriteLines(output, Usage)
			return Request{}, command.ExitUsage, false
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(positionals[0])
		if err != nil || len(raw) != 16 {
			command.WriteLines(output, "Account: invalid credential ID")
			return Request{}, command.ExitUsage, false
		}
		return Request{action: action, credential: [16]byte(raw)}, command.ExitOK, true
	default:
		command.WriteLines(output, Usage)
		return Request{}, command.ExitUsage, false
	}
}

// parseRevokePositionals enforces revoke's option-free positional contract:
// Base64URL credential IDs can legitimately start with "-", so non-help tokens
// before "--" stay positional instead of failing as unknown options.
func parseRevokePositionals(args []string) ([]string, command.ParseOutcome) {
	positionals := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			return append(positionals, args[i+1:]...), command.ParseReady
		}
		if command.IsHelpFlag(arg) {
			return nil, command.ParseHelp
		}
		positionals = append(positionals, arg)
	}
	return positionals, command.ParseReady
}

func projectAccountFlagParse(output command.LineOutput, label string, outcome command.ParseOutcome) (command.ExitCode, bool) {
	switch outcome {
	case command.ParseReady:
		return command.ExitOK, true
	case command.ParseHelp:
		command.WriteLines(output, Usage)
		return command.ExitOK, false
	default:
		command.WriteLinef(output, "%s: %s", label, outcome.Diagnostic())
		return outcome.ExitCode(), false
	}
}

func (runner Runner) Run(ctx context.Context, args []string) command.ExitCode {
	request, code, ready := Parse(args, runner.Output)
	if !ready {
		return code
	}
	return runner.Execute(ctx, request)
}

func (runner Runner) Execute(ctx context.Context, request Request) command.ExitCode {
	if runner.Service == nil {
		command.WriteLines(runner.Output, account.ErrNotConfigured.Error())
		return command.ExitFailure
	}
	if request.action == "status" && request.jsonOutput {
		snapshot, _ := runner.Service.Snapshot()
		if err := json.NewEncoder(runner.JSONOutput).Encode(snapshot); err != nil {
			return command.ExitFailure
		}
		return command.ExitOK
	}
	if err := runner.execute(ctx, request); err != nil {
		if usage, ok := errors.AsType[usageError](err); ok {
			command.WriteLines(runner.Output, usage.message)
			return command.ExitUsage
		}
		command.WriteLines(runner.Output, "Account: "+err.Error())
		return command.ExitFailure
	}
	snapshot, _ := runner.Service.Snapshot()
	if request.action == "verify" {
		command.WriteLines(runner.Output, "Account setup complete.")
	} else {
		command.WriteLines(runner.Output, Describe(snapshot))
	}
	if identity := IdentityLines(snapshot); len(identity) > 0 {
		command.WriteLines(runner.Output, identity...)
	}
	return command.ExitOK
}
func (runner Runner) execute(ctx context.Context, request Request) error {
	switch request.action {
	case "register":
		return runner.register(ctx)
	case "status":
		return nil
	case "refresh":
		return runner.refresh(ctx)
	case "login":
		password, err := runner.password(ctx, request.passwordStdin)
		if err != nil {
			return err
		}
		return runner.Service.Login(ctx, request.value, password)
	case "password":
		return runner.setPassword(ctx, request.value, request.passwordStdin)
	case "email":
		return runner.Service.RequestEmail(ctx, request.value)
	case "verify":
		return runner.Service.VerifyEmail(ctx, request.value)
	case "revoke":
		return runner.revoke(ctx, request.credential)
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
			case "account_storage_error":
				return errors.New("local account file is unreadable or invalid; repair or remove it and try again")
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

func (runner Runner) revoke(ctx context.Context, credential [16]byte) error {
	snapshot, _ := runner.Service.Snapshot()
	if snapshot.Account == "" {
		return errors.New("account setup is still pending")
	}
	return runner.Service.RevokeCredential(ctx, snapshot.Account, credential)
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
