package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relaysetup"
)

const relayUsage = "Usage: wind relay add <url> [--name <device>] [--no-browser] | wind relay onboard <url> [--name <device>] [--no-browser] | wind relay cancel <url>"

func (app *App) runRelay(ctx context.Context, args []string) int {
	if len(args) < 2 {
		app.writeCompleteLine("%s", relayUsage)
		return ExitUsage
	}
	operation := relaysetup.AddProfile
	switch args[0] {
	case "add":
	case "onboard":
		operation = relaysetup.PrivateOnboarding
	case "cancel":
		if len(args) != 2 {
			app.writeCompleteLine("%s", relayUsage)
			return ExitUsage
		}
		if err := app.application.CancelRelaySetup(ctx, args[1]); err != nil {
			app.writeCompleteLine("relay cancel: %v", err)
			return ExitFailure
		}
		app.writeCompleteLine("Pending relay setup cancelled.")
		return ExitOK
	default:
		app.writeCompleteLine("%s", relayUsage)
		return ExitUsage
	}
	flags := flag.NewFlagSet("relay "+args[0], flag.ContinueOnError)
	flags.SetOutput(app.stderrWriter())
	flags.Usage = func() {
		_, _ = io.WriteString(app.stderrWriter(), "Usage of "+flags.Name()+":\n"+flagDefaults(flags))
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "WindShare client"
	}
	noBrowser := flags.Bool("no-browser", false, "display the approval URL without opening a browser")
	name := flags.String("name", hostname, "device name displayed for browser approval")
	flagArgs := args[2:]
	if isFlagHelpArgument(args[1]) {
		// `relay add --help` has no URL yet; show the same help as `relay add <url> --help`.
		flagArgs = args[1:]
	}
	if err := flags.Parse(flagArgs); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	current, err := app.application.StartRelaySetup(ctx, engine.RelaySetupRequest{URL: args[1], DeviceName: *name, Operation: operation, NoBrowser: *noBrowser})
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
			app.writeCompleteLine("Setup was saved. Run the same command to continue, or wind relay cancel %s.", args[1])
		}
		return ExitFailure
	}
	app.writeCompleteLine("Relay saved: %s", result.Value.RelayURL)
	if operation == relaysetup.PrivateOnboarding {
		app.writeCompleteLine("Only this private relay is selected. P2P remains enabled.")
	}
	if result.Value.FrontURL == "" {
		app.writeCompleteLine("No share frontend was advertised. Pass --front-url when sharing.")
	}
	return ExitOK
}

// isFlagHelpArgument reports whether argument is one of the bare help flags
// -h, -help, --h or --help. Valued forms such as -h=true or --help=1, which the
// flag package also treats as a help request, are not recognized, so in the
// first operand position they are still taken as the URL.
func isFlagHelpArgument(argument string) bool {
	switch argument {
	case "-h", "-help", "--h", "--help":
		return true
	}
	return false
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
