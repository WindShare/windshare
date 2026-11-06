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
	// SupportsSetup reports whether this host can apply executable firewall
	// rules. Explicit setup prints the Windows consent preamble only when true.
	SupportsSetup() bool
}

// LineOutput commits complete diagnostic records immediately. Embedded line
// breaks belong to the record; a trailing newline is not required.
type LineOutput interface {
	WriteLines(...string)
}

type Runner struct {
	Service     Service
	Output      LineOutput
	JSONOutput  io.Writer
	Interactive func() bool
}

func (runner Runner) Run(ctx context.Context, command string, args []string) int {
	if runner.JSONOutput == nil {
		runner.JSONOutput = io.Discard
	}
	options, err := parseNetworkArgs(command, args)
	if err != nil {
		usage := "Usage: wind " + command + " network [--json]"
		if command == "setup" {
			usage += " [--skip|--remove] [--non-interactive]"
		}
		runner.writeLine("%s", usage)
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
			runner.writeLine("Network preference: %v", err)
			return ResultFailure
		}
		if options.jsonOutput {
			fmt.Fprintln(runner.JSONOutput, `{"choice":"skip"}`)
		} else {
			runner.writeLine("Network setup reminders disabled.")
		}
		return ResultOK
	}
	action := platformsetup.EnsureRules
	if options.remove {
		action = platformsetup.RemoveRules
	}
	// Explicit setup is the authorization to request UAC. Scope is printed before
	// Windows asks for consent, and redirected/CI execution never opens a prompt.
	// Unsupported platforms cannot apply rules, so the Windows-specific consent
	// text would only confuse the later OperationUnsupported result.
	if !options.jsonOutput && runner.Service.SupportsSetup() {
		if options.remove {
			runner.writeLine("Remove this executable's WindShare firewall rules.")
		} else {
			runner.writeLine("Allow inbound UDP/TCP for this executable on domain, private, and public networks (all ports and remote addresses).")
		}
	}
	// Elevation depends on the host's raw terminal handles, not its line sink.
	allowElevation := !options.noninteractive && !options.jsonOutput && runner.Interactive != nil && runner.Interactive()
	operation, report := runner.Service.Setup(ctx, action, allowElevation)
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
		runner.writeLine("Rule operation: %s%s", operation.State, parenthesized(operation.Detail))
	}
	runner.writeLine("Executable: %s\nSetup choice: %s\nConfigured executable: %s\nScope: %s\nGlobal rules: not evaluated", report.Executable, report.Preference.Choice, report.Preference.Executable, platformsetup.InspectionScope)
	if report.PreferenceError != "" {
		runner.writeLine("Preference: unknown (%s)", report.PreferenceError)
	}
	if report.InspectionError != "" {
		runner.writeLine("Inspection: unknown (%s)", report.InspectionError)
	}
	for _, finding := range report.Findings {
		runner.writeLine("%s", formatFindingLine(finding))
		for _, rule := range finding.Rules {
			runner.writeLine("  %s: %s%s", rule.Name, rule.Action, parenthesized(rule.Restrictions))
		}
	}
	if report.SuggestSetup() {
		runner.writeLine("Configure: wind setup network")
	}
}

func (runner Runner) writeLine(format string, args ...any) {
	if runner.Output != nil {
		runner.Output.WriteLines(fmt.Sprintf(format, args...))
	}
}

func parenthesized(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// formatFindingLine omits an empty interface so unsupported-platform findings
// read as "UDP: unknown (unsupported-platform)" instead of a leading space.
func formatFindingLine(finding platformsetup.Finding) string {
	parts := make([]string, 0, 5)
	if finding.Network.Interface != "" {
		parts = append(parts, finding.Network.Interface, finding.Network.Profile.String())
	} else if finding.Network.Profile != platformsetup.UnknownProfile {
		parts = append(parts, finding.Network.Profile.String())
	}
	parts = append(parts, finding.Protocol.String()+":", finding.Status.String(), "("+finding.Reason.String()+")")
	return strings.Join(parts, " ")
}
