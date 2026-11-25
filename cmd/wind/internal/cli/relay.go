package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

// Relay synopses are the single source of truth shared between `wind relay`
// help and top-level `wind` usage so subcommands cannot drift out of sync.
const (
	relayAddSynopsis        = command.Name + " relay add <url> [--access-key <key>] [--name <device>] [--no-browser]"
	relayOnboardSynopsis    = command.Name + " relay onboard <url> [--name <device>] [--no-browser]"
	relayPreferenceSynopsis = command.Name + " relay list | mode <automatic|selected> | enable|disable|remove <url> | frontend <url|default>"
	relayCancelSynopsis     = command.Name + " relay cancel <url>"
)

const relayUsage = "Usage:\n" +
	"  " + relayAddSynopsis + "\n" +
	"  " + relayOnboardSynopsis + "\n" +
	"  " + relayPreferenceSynopsis + "\n" +
	"  " + relayCancelSynopsis

func (app *App) runRelay(ctx context.Context, args []string) (code int) {
	if len(args) == 0 {
		app.writeCompleteLine("%s", relayUsage)
		return ExitUsage
	}
	if command.IsHelpToken(args[0]) {
		app.writeCompleteLine("%s", relayUsage)
		return ExitOK
	}
	switch args[0] {
	case "add", "onboard":
		request, outcome := app.parseRelaySetupRequest(args[0], args[1:])
		if outcome != requestParseReady {
			return outcome.exitCode()
		}
		return app.runRelaySetup(ctx, request)
	case "list", "mode", "enable", "disable", "remove", "frontend":
		return app.relayPreferences(ctx, args)
	case "cancel":
		flags := command.NewFlagSet("relay cancel")
		address, outcome := app.parseRelayURL(flags, "relay cancel <url>", args[1:])
		if outcome != requestParseReady {
			return outcome.exitCode()
		}
		closeApp, err := app.ensureApplication(diagnostics.Sink{})
		if err != nil {
			return ExitFailure
		}
		defer app.closeCommandApplication(closeApp, &code)
		if err := app.application.CancelRelaySetup(ctx, address); err != nil {
			app.writeCompleteLine("relay cancel: %v", err)
			return relayCancelExitCode(err)
		}
		app.writeCompleteLine("Pending relay setup cancelled.")
		return ExitOK
	default:
		app.writeCompleteLine("%s", relayUsage)
		return ExitUsage
	}
}

type relaySetupCommand struct {
	request   engine.RelaySetupRequest
	noBrowser bool
}

func (app *App) parseRelaySetupRequest(operation string, args []string) (relaySetupCommand, requestParseOutcome) {
	cmd := relaySetupCommand{request: engine.RelaySetupRequest{Operation: relaysetup.AddProfile}}
	if operation == "onboard" {
		cmd.request.Operation = relaysetup.AuthorizeDevice
	}
	flags := command.NewFlagSet("relay " + operation)
	if operation == "add" {
		flags.StringVar(&cmd.request.AccessKey, "access-key", "", "merge this service key into the saved relay")
	}
	hostname, _ := os.Hostname()
	flags.BoolVar(&cmd.noBrowser, "no-browser", false, "display the approval URL without opening a browser")
	flags.StringVar(&cmd.request.DeviceName, "name", relaysetup.DefaultDeviceName(hostname), "device name displayed for browser approval")
	address, outcome := app.parseRelayURL(flags, flags.Name()+" <url> [options]", args)
	if outcome != requestParseReady {
		return relaySetupCommand{}, outcome
	}
	if err := relaysetup.ValidateDeviceName(cmd.request.DeviceName); err != nil {
		app.writeRelaySetupFailure(err)
		return relaySetupCommand{}, requestParseUsageFailure
	}
	cmd.request.URL = address
	return cmd, requestParseReady
}

func (app *App) parseRelayURL(flags *flag.FlagSet, synopsis string, args []string) (string, requestParseOutcome) {
	positionals, flagParse := command.ParseInterleaved(flags, args)
	if outcome := app.projectFlagParse(flags.Name(), flags, synopsis, flagParse); outcome != requestParseReady {
		return "", outcome
	}
	if len(positionals) != 1 || positionals[0] == "" {
		app.writeCompleteLine("%s: exactly one relay URL is required", flags.Name())
		return "", requestParseUsageFailure
	}
	return positionals[0], requestParseReady
}

func (app *App) runRelaySetup(ctx context.Context, command relaySetupCommand) (code int) {
	closeApp, err := app.ensureApplication(diagnostics.Sink{})
	if err != nil {
		return ExitFailure
	}
	defer app.closeCommandApplication(closeApp, &code)
	current, err := app.application.StartRelaySetup(ctx, command.request)
	if err != nil {
		return app.writeRelaySetupFailure(err)
	}
	for observation := range current.Observations() {
		if event, ok := observation.Event.(engine.RelaySetupProgress); ok && event.Stage == "approval_required" {
			app.presentRelayApproval(ctx, event, command.noBrowser)
		}
	}
	result, err := current.Wait(context.Background())
	if err != nil {
		return app.writeRelaySetupFailure(err)
	}
	if result.Err != nil {
		code := app.writeRelaySetupFailure(result.Err)
		// A concrete input error takes precedence over cancellation: retrying
		// the unchanged command would fail again.
		if code != ExitUsage && ctx.Err() != nil {
			app.writeInterruptedRelaySetupHint(result.Err)
		}
		return code
	}
	receipt := result.Value
	mode := relayconfig.Automatic
	if receipt.ConfiguredOnly {
		mode = relayconfig.Selected
	}
	app.writeCompleteLine("Relay saved: %s (%s)", receipt.RelayURL, relayEnabledLabel(receipt.Enabled))
	front := receipt.FrontURL
	if front == "" {
		front = app.defaultFrontend()
	}
	app.writeCompleteLine("Relay mode: %s. Share website: %s", mode, front)
	if receipt.OfferedFrontend != "" && receipt.OfferedFrontend != front {
		app.writeCompleteLine("Use your receiver website: wind relay frontend %s", receipt.OfferedFrontend)
	}
	return ExitOK
}

func (app *App) presentRelayApproval(ctx context.Context, event engine.RelaySetupProgress, noBrowser bool) {
	if event.ServiceLogin != nil {
		// Match Desktop: when both service-account sign-in and owner approval
		// are available, display the discovered login host and let the user
		// choose instead of auto-opening one destination.
		app.writeCompleteLine(
			"Sign in with %s: %s\nOr pair as node owner: %s\nDevice code: %s",
			event.ServiceLogin.Host, event.ServiceLogin.URL, event.VerificationURL, event.UserCode,
		)
		return
	}
	app.writeCompleteLine("Approve this device: %s\nDevice code: %s", event.VerificationURL, event.UserCode)
	if !noBrowser && app.openBrowser != nil {
		if err := app.openBrowser(ctx, event.VerificationURL); err != nil && ctx.Err() == nil {
			app.writeCompleteLine("Open the approval URL on another device and confirm the code.")
		}
	}
}

func (app *App) writeRelaySetupFailure(err error) int {
	app.writeCompleteLine("relay setup: %v", err)
	if errors.Is(err, relaysetup.ErrInvalidDeviceName) {
		app.writeCompleteLine("Choose another device name with --name.")
		return ExitUsage
	}
	return ExitFailure
}

// relayCancelExitCode treats "nothing is pending" as a user error, like naming
// an --only path or a resume item that does not exist: the command named
// nothing to act on, and retrying it unchanged cannot succeed.
func relayCancelExitCode(err error) int {
	if errors.Is(err, relaysetup.ErrNoPendingSetup) {
		return ExitUsage
	}
	return ExitFailure
}

func (app *App) writeInterruptedRelaySetupHint(err error) {
	var failure *relaysetup.SetupError
	if !errors.As(err, &failure) {
		return
	}
	recovery := failure.Recovery
	switch recovery.Action {
	case relaysetup.RecoveryRestart:
		app.writeCompleteLine("No pending setup was recorded; run the same command to start again.")
	case relaysetup.RecoveryContinue:
		app.writeCompleteLine("Setup was saved. Run the same command to continue, or %s relay cancel %s.", command.Name, recovery.RelayURL)
	case relaysetup.RecoveryCancel:
		app.writeCompleteLine("Cancel the saved setup with %s relay cancel %s, then run the same command again.", command.Name, recovery.RelayURL)
	case relaysetup.RecoveryRetryCancel:
		app.writeCompleteLine("Cancellation is unfinished; run %s relay cancel %s to finish it.", command.Name, recovery.RelayURL)
	case relaysetup.RecoveryBlocked:
		app.writeCompleteLine("Finish the pending setup for %s, or run %s relay cancel %s.", recovery.RelayURL, command.Name, recovery.RelayURL)
	default:
		app.writeCompleteLine("Setup state could not be confirmed.")
	}
}

func openRelayBrowser(ctx context.Context, address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", address)
	case "darwin":
		command = exec.CommandContext(ctx, "open", address)
	default:
		command = exec.CommandContext(ctx, "xdg-open", address)
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("open approval browser: %w", err)
	}
	return nil
}
