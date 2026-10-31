package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relaysetup"
)

const relayUsage = "Usage: wind relay add <url> [--name <device>] [--no-browser] | wind relay onboard <url> [--name <device>] [--no-browser] | wind relay cancel <url>"

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
	case "cancel":
		flags := app.newFlagSet("relay cancel")
		address, outcome := app.parseRelayURL(flags, "relay cancel <url>", args[1:])
		if outcome != requestParseReady {
			return outcome.exitCode()
		}
		if err := app.application.CancelRelaySetup(ctx, address); err != nil {
			app.writeCompleteLine("relay cancel: %v", err)
			return ExitFailure
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
		request.Operation = relaysetup.PrivateOnboarding
	}
	flags := app.newFlagSet("relay " + operation)
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "WindShare client"
	}
	flags.BoolVar(&request.NoBrowser, "no-browser", false, "display the approval URL without opening a browser")
	flags.StringVar(&request.DeviceName, "name", hostname, "device name displayed for browser approval")
	address, outcome := app.parseRelayURL(flags, flags.Name()+" <url> [options]", args)
	if outcome != requestParseReady {
		return engine.RelaySetupRequest{}, outcome
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
		app.writeCompleteLine("relay setup: %v", err)
		return ExitFailure
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
		app.writeCompleteLine("relay setup: %v", err)
		return ExitFailure
	}
	if result.Err != nil {
		app.writeCompleteLine("relay setup: %v", result.Err)
		if ctx.Err() != nil {
			app.writeCompleteLine("Setup was saved. Run the same command to continue, or wind relay cancel %s.", request.URL)
		}
		return ExitFailure
	}
	app.writeCompleteLine("Relay saved: %s", result.Value.RelayURL)
	if request.Operation == relaysetup.PrivateOnboarding {
		app.writeCompleteLine("Only this private relay is selected. P2P remains enabled.")
	}
	if result.Value.FrontURL == "" {
		app.writeCompleteLine("No share frontend was advertised. Pass --front-url when sharing.")
	}
	return ExitOK
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
