// Package networkcommand owns explicit setup and read-only network diagnostics.
// It has no transfer engine authority: changing a host rule cannot change a task's
// selected connectivity mode or its retry behavior.
package networkcommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/internal/platformsetup"
)

type Service interface {
	Check(context.Context) platformsetup.Report
	Setup(context.Context, platformsetup.RuleAction, bool) (platformsetup.Operation, platformsetup.Report)
	Skip() error
	// SupportsSetup reports whether this host can apply executable firewall
	// rules. Explicit setup prints the Windows consent preamble only when true.
	SupportsSetup() bool
}

type Runner struct {
	Service     Service
	Output      command.LineOutput
	JSONOutput  io.Writer
	Interactive func() bool
}

// DoctorSynopsis and SetupSynopsis are the single source of truth for network
// command lines shared between subcommand usage and top-level wind usage.
const (
	DoctorSynopsis = command.Name + " doctor network [--json]"
	SetupSynopsis  = command.Name + " setup network [--skip|--remove] [--non-interactive] [--json]"
)

func networkUsage(cmd string) string {
	switch cmd {
	case "doctor":
		return "Usage: " + DoctorSynopsis
	case "setup":
		return "Usage: " + SetupSynopsis
	default:
		return "Usage:\n  " + DoctorSynopsis + "\n  " + SetupSynopsis
	}
}

func (runner Runner) Run(ctx context.Context, cmd string, args []string) command.ExitCode {
	if runner.JSONOutput == nil {
		runner.JSONOutput = io.Discard
	}
	options, err := parseNetworkArgs(cmd, args)
	if err != nil {
		return runner.reportParseError(cmd, args, err)
	}
	if runner.Service == nil {
		runner.Service = platformsetup.New()
	}
	if cmd == "doctor" {
		report := runner.Service.Check(ctx)
		runner.report(report, nil, options.jsonOutput)
		if report.InspectionError != "" || report.PreferenceError != "" {
			return command.ExitFailure
		}
		return command.ExitOK
	}
	if options.skip {
		if err := runner.Service.Skip(); err != nil {
			runner.writeLine("Network preference: %v", err)
			return command.ExitFailure
		}
		if options.jsonOutput {
			fmt.Fprintln(runner.JSONOutput, `{"choice":"skip"}`)
		} else {
			runner.writeLine("Network setup reminders disabled.")
		}
		return command.ExitOK
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
		return command.ExitFailure
	}
	return command.ExitOK
}

type networkOptions struct{ skip, remove, noninteractive, jsonOutput bool }

var errNetworkUsage = errors.New("network usage")

func (runner Runner) reportParseError(cmd string, args []string, err error) command.ExitCode {
	if errors.Is(err, command.ErrHelp) {
		runner.writeLine("%s", networkUsage(cmd))
		return command.ExitOK
	}
	if !errors.Is(err, errNetworkUsage) {
		label := cmd
		if cmd != "doctor" && cmd != "setup" {
			label = command.Name
		} else if len(args) > 0 && args[0] == "network" {
			label = cmd + " network"
		}
		runner.writeLine("%s: %v", label, err)
	}
	runner.writeLine("%s", networkUsage(cmd))
	return command.ParseExitCode(err)
}

func parseNetworkArgs(cmd string, args []string) (networkOptions, error) {
	var options networkOptions
	if cmd != "doctor" && cmd != "setup" {
		return options, fmt.Errorf("unknown command %q", command.RedactToken(cmd))
	}
	if len(args) == 0 {
		return options, errNetworkUsage
	}
	if command.IsHelpToken(args[0]) {
		return options, command.ErrHelp
	}
	if args[0] != "network" {
		return options, fmt.Errorf("unknown target %q", command.RedactToken(args[0]))
	}
	flags := command.NewFlagSet(cmd + " network")
	flags.BoolVar(&options.jsonOutput, "json", false, "write the executable rule inspection as JSON")
	if cmd == "setup" {
		flags.BoolVar(&options.skip, "skip", false, "save the preference to stop reminders")
		flags.BoolVar(&options.remove, "remove", false, "remove only this executable's WindShare rules")
		flags.BoolVar(&options.noninteractive, "non-interactive", false, "never request elevation")
	}
	if err := command.ParseNoPositionals(flags, args[1:]); err != nil {
		return options, err
	}
	if options.skip && options.remove {
		return options, errors.New("--skip and --remove cannot be used together")
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
		runner.writeLine("Configure: %s setup network", command.Name)
	}
}

func (runner Runner) writeLine(format string, args ...any) {
	command.WriteLinef(runner.Output, format, args...)
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
