// Package networkcommand owns explicit setup and read-only network diagnostics.
// It has no transfer engine authority: changing a host rule cannot change a task's
// selected connectivity mode or its retry behavior.
package networkcommand

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/windshare/windshare/internal/platformsetup"
)

const (
	ResultOK      = 0
	ResultFailure = 1
	ResultUsage   = 2
)

type Service interface {
	Check(context.Context) platformsetup.Report
	Setup(context.Context, platformsetup.RuleAction, bool) (platformsetup.Operation, platformsetup.Report)
	Skip() error
}

type Runner struct {
	Service     Service
	Input       io.Reader
	Output      io.Writer
	JSONOutput  io.Writer
	Interactive func() bool
}

func (runner Runner) Run(ctx context.Context, command string, args []string) int {
	if runner.JSONOutput == nil {
		runner.JSONOutput = runner.Output
	}
	options, err := parseNetworkArgs(command, args)
	if err != nil {
		fmt.Fprintf(runner.Output, "Usage: wind %s network [--json]", command)
		if command == "setup" {
			fmt.Fprint(runner.Output, " [--skip|--remove] [--non-interactive]")
		}
		fmt.Fprintln(runner.Output)
		if errors.Is(err, flag.ErrHelp) {
			return ResultOK
		}
		return ResultUsage
	}
	if runner.Service == nil {
		runner.Service = platformsetup.New()
	}
	if command == "doctor" {
		report := runner.Service.Check(ctx)
		runner.report(report, nil, options.jsonOutput)
		if report.InspectionError != "" || report.PreferenceError != "" {
			return ResultFailure
		}
		return ResultOK
	}
	if options.skip {
		if err := runner.Service.Skip(); err != nil {
			fmt.Fprintf(runner.Output, "Network preference: %v\n", err)
			return ResultFailure
		}
		if options.jsonOutput {
			fmt.Fprintln(runner.JSONOutput, `{"choice":"skip"}`)
		} else {
			fmt.Fprintln(runner.Output, "Network setup reminders disabled.")
		}
		return ResultOK
	}
	action := platformsetup.EnsureRules
	if options.remove {
		action = platformsetup.RemoveRules
	}
	// Explicit setup is the authorization to request UAC. Scope is printed before
	// Windows asks for consent, and redirected/CI execution never opens a prompt.
	if !options.jsonOutput {
		if options.remove {
			fmt.Fprintln(runner.Output, "Remove this executable's WindShare firewall rules.")
		} else {
			fmt.Fprintln(runner.Output, "Allow inbound UDP/TCP for this executable on domain, private, and public networks (all ports and remote addresses).")
		}
	}
	interactive := runner.Interactive
	if interactive == nil {
		interactive = func() bool { return IsInteractive(runner.Input, runner.Output) }
	}
	operation, report := runner.Service.Setup(ctx, action, !options.noninteractive && !options.jsonOutput && interactive())
	runner.report(report, &operation, options.jsonOutput)
	if !operation.Succeeded() || report.PreferenceError != "" {
		return ResultFailure
	}
	return ResultOK
}

type networkOptions struct{ skip, remove, noninteractive, jsonOutput bool }

func parseNetworkArgs(command string, args []string) (networkOptions, error) {
	var options networkOptions
	if len(args) == 0 || args[0] != "network" || (command != "doctor" && command != "setup") {
		return options, errors.New("invalid network command")
	}
	flags := flag.NewFlagSet(command+" network", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	flags.BoolVar(&options.jsonOutput, "json", false, "write the executable rule inspection as JSON")
	if command == "setup" {
		flags.BoolVar(&options.skip, "skip", false, "save the preference to stop reminders")
		flags.BoolVar(&options.remove, "remove", false, "remove only this executable's WindShare rules")
		flags.BoolVar(&options.noninteractive, "non-interactive", false, "never request elevation")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return options, err
	}
	if flags.NArg() != 0 || options.skip && options.remove {
		return options, errors.New("invalid network options")
	}
	return options, nil
}

func IsInteractive(input io.Reader, output io.Writer) bool {
	if value := strings.ToLower(os.Getenv("CI")); value != "" && value != "false" && value != "0" {
		return false
	}
	in, inputOK := input.(interface{ Fd() uintptr })
	out, outputOK := output.(interface{ Fd() uintptr })
	return inputOK && outputOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func (runner Runner) report(report platformsetup.Report, operation *platformsetup.Operation, jsonOutput bool) {
	if jsonOutput {
		_ = json.NewEncoder(runner.JSONOutput).Encode(newJSONReport(report, operation))
		return
	}
	if operation != nil {
		fmt.Fprintf(runner.Output, "Rule operation: %s", operation.State)
		if operation.Detail != "" {
			fmt.Fprintf(runner.Output, " (%s)", operation.Detail)
		}
		fmt.Fprintln(runner.Output)
	}
	fmt.Fprintf(runner.Output, "Executable: %s\nSetup choice: %s\nConfigured executable: %s\nScope: %s\nGlobal rules: not evaluated\n", report.Executable, report.Preference.Choice, report.Preference.Executable, platformsetup.InspectionScope)
	if report.PreferenceError != "" {
		fmt.Fprintf(runner.Output, "Preference: unknown (%s)\n", report.PreferenceError)
	}
	if report.InspectionError != "" {
		fmt.Fprintf(runner.Output, "Inspection: unknown (%s)\n", report.InspectionError)
	}
	for _, finding := range report.Findings {
		fmt.Fprintf(runner.Output, "%s %s %s: %s (%s)\n", finding.Network.Interface, finding.Network.Profile, finding.Protocol, finding.Status, finding.Reason)
		for _, rule := range finding.Rules {
			fmt.Fprintf(runner.Output, "  %s: %s", rule.Name, rule.Action)
			if rule.Restrictions != "" {
				fmt.Fprintf(runner.Output, " (%s)", rule.Restrictions)
			}
			fmt.Fprintln(runner.Output)
		}
	}
	if report.SuggestSetup() {
		fmt.Fprintln(runner.Output, "Configure: wind setup network")
	}
}
