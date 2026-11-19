package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/windshare/windshare/cmd/wind/internal/commandmeta"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

const relayUsage = "Usage: wind relay add <url> [--access-key <key>] [--name <device>] [--no-browser] | wind relay onboard <url> [--name <device>] [--no-browser] | wind relay cancel <url> | wind relay list | wind relay mode <automatic|selected> | wind relay enable|disable <url> | wind relay frontend <url|default>"

func (app *App) runRelay(ctx context.Context, args []string) int {
	if len(args) == 0 {
		app.writeCompleteLine("%s", relayUsage)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		app.writeCompleteLine("%s", relayUsage)
		return ExitOK
	case "add", "onboard":
		request, outcome := app.parseRelaySetupRequest(args[0], args[1:])
		if outcome != requestParseReady {
			return outcome.exitCode()
		}
		return app.runRelaySetup(ctx, request)
	case "list", "mode", "enable", "disable", "frontend":
		return app.relayPreferences(ctx, args)
	case "cancel":
		flags := app.newFlagSet("relay cancel")
		address, outcome := app.parseRelayURL(flags, "relay cancel <url>", args[1:])
		if outcome != requestParseReady {
			return outcome.exitCode()
		}
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

func (app *App) parseRelaySetupRequest(operation string, args []string) (engine.RelaySetupRequest, requestParseOutcome) {
	request := engine.RelaySetupRequest{Operation: relaysetup.AddProfile}
	if operation == "onboard" {
		request.Operation = relaysetup.AuthorizeDevice
	}
	flags := app.newFlagSet("relay " + operation)
	if operation == "add" {
		flags.StringVar(&request.AccessKey, "access-key", "", "merge this service key into the saved relay")
	}
	hostname, _ := os.Hostname()
	flags.BoolVar(&request.NoBrowser, "no-browser", false, "display the approval URL without opening a browser")
	flags.StringVar(&request.DeviceName, "name", relaysetup.DefaultDeviceName(hostname), "device name displayed for browser approval")
	address, outcome := app.parseRelayURL(flags, flags.Name()+" <url> [options]", args)
	if outcome != requestParseReady {
		return engine.RelaySetupRequest{}, outcome
	}
	if err := relaysetup.ValidateDeviceName(request.DeviceName); err != nil {
		app.writeRelaySetupFailure(err)
		return engine.RelaySetupRequest{}, requestParseUsageFailure
	}
	request.URL = address
	return request, requestParseReady
}

func (app *App) parseRelayURL(flags *flag.FlagSet, synopsis string, args []string) (string, requestParseOutcome) {
	positionals, flagParse := parseInterleaved(flags, args)
	if outcome := app.projectFlagParse(flags.Name(), flags, synopsis, flagParse); outcome != requestParseReady {
		return "", outcome
	}
	if len(positionals) != 1 || positionals[0] == "" {
		app.writeCompleteLine("%s: exactly one relay URL is required", flags.Name())
		return "", requestParseUsageFailure
	}
	return positionals[0], requestParseReady
}

func (app *App) runRelaySetup(ctx context.Context, request engine.RelaySetupRequest) int {
	current, err := app.application.StartRelaySetup(ctx, request)
	if err != nil {
		return app.writeRelaySetupFailure(err)
	}
	for observation := range current.Observations() {
		if event, ok := observation.Event.(engine.RelaySetupProgress); ok {
			switch event.Stage {
			case "approval_required":
				app.writeCompleteLine("Approve this device: %s\nDevice code: %s", event.VerificationURL, event.UserCode)
			case "browser_unavailable":
				app.writeCompleteLine("Open the approval URL on another device and confirm the code.")
			}
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
		app.writeCompleteLine("Setup was saved. Run the same command to continue, or %s relay cancel %s.", commandmeta.Name, recovery.RelayURL)
	case relaysetup.RecoveryCancel:
		app.writeCompleteLine("Cancel the saved setup with %s relay cancel %s, then run the same command again.", commandmeta.Name, recovery.RelayURL)
	case relaysetup.RecoveryRetryCancel:
		app.writeCompleteLine("Cancellation is unfinished; run %s relay cancel %s to finish it.", commandmeta.Name, recovery.RelayURL)
	case relaysetup.RecoveryBlocked:
		app.writeCompleteLine("Finish the pending setup for %s, or run %s relay cancel %s.", recovery.RelayURL, commandmeta.Name, recovery.RelayURL)
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
