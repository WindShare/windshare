package networkcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/internal/platformsetup"
)

type recordedLines []string

func (output *recordedLines) WriteLines(lines ...string) {
	*output = append(*output, lines...)
}

type fakeService struct {
	checks, setups, skips int
	elevation             bool
	action                platformsetup.RuleAction
	report                platformsetup.Report
	operation             platformsetup.Operation
	skipErr               error
	supportsSetup         bool
}

func (service *fakeService) Check(context.Context) platformsetup.Report {
	service.checks++
	return service.report
}
func (service *fakeService) Setup(_ context.Context, action platformsetup.RuleAction, elevation bool) (platformsetup.Operation, platformsetup.Report) {
	service.setups++
	service.action, service.elevation = action, elevation
	return service.operation, service.report
}
func (service *fakeService) Skip() error         { service.skips++; return service.skipErr }
func (service *fakeService) SupportsSetup() bool { return service.supportsSetup }

func TestCommandsKeepReadingPreferenceAndMutationIndependent(t *testing.T) {
	for _, test := range []struct {
		command               string
		args                  []string
		checks, setups, skips int
		elevation             bool
		action                platformsetup.RuleAction
		code                  command.ExitCode
	}{
		{"doctor", []string{"network"}, 1, 0, 0, false, 0, command.ExitOK},
		{"doctor", []string{"network", "--json"}, 1, 0, 0, false, 0, command.ExitOK},
		{"setup", []string{"network"}, 0, 1, 0, true, platformsetup.EnsureRules, command.ExitOK},
		{"setup", []string{"network", "--non-interactive"}, 0, 1, 0, false, platformsetup.EnsureRules, command.ExitOK},
		{"setup", []string{"network", "--json"}, 0, 1, 0, false, platformsetup.EnsureRules, command.ExitOK},
		{"setup", []string{"network", "--remove"}, 0, 1, 0, true, platformsetup.RemoveRules, command.ExitOK},
		{"setup", []string{"network", "--skip"}, 0, 0, 1, false, 0, command.ExitOK},
		{"setup", []string{"network", "--skip", "--json"}, 0, 0, 1, false, 0, command.ExitOK},
		{"setup", []string{"network", "--skip", "--remove"}, 0, 0, 0, false, 0, command.ExitUsage},
		{"doctor", []string{"network", "--skip"}, 0, 0, 0, false, 0, command.ExitUsage},
		{"doctor", []string{"network", "extra"}, 0, 0, 0, false, 0, command.ExitUsage},
		{"doctor", []string{"--help"}, 0, 0, 0, false, 0, command.ExitOK},
		{"doctor", []string{"help"}, 0, 0, 0, false, 0, command.ExitOK},
		{"setup", []string{"--help"}, 0, 0, 0, false, 0, command.ExitOK},
		{"setup", []string{"help"}, 0, 0, 0, false, 0, command.ExitOK},
		{"setup", []string{"network", "--help"}, 0, 0, 0, false, 0, command.ExitOK},
		{"setup", nil, 0, 0, 0, false, 0, command.ExitUsage},
	} {
		service := &fakeService{supportsSetup: true, operation: platformsetup.Operation{State: platformsetup.RulesApplied}}
		runner := Runner{Service: service, Output: &recordedLines{}, Interactive: func() bool { return true }}
		code := runner.Run(t.Context(), test.command, test.args)
		if code != test.code || service.checks != test.checks || service.setups != test.setups || service.skips != test.skips || service.elevation != test.elevation || service.action != test.action {
			t.Fatalf("%s %v: code=%d, service=%+v", test.command, test.args, code, service)
		}
	}
}

func TestCommandFailuresRemainSeparateFromPolicyVerdicts(t *testing.T) {
	for _, state := range []platformsetup.OperationState{platformsetup.AuthorizationDeclined, platformsetup.PermissionRequired, platformsetup.OperationFailed, platformsetup.OperationUnsupported} {
		service := &fakeService{supportsSetup: state != platformsetup.OperationUnsupported, operation: platformsetup.Operation{State: state, Detail: "failure"}}
		runner := Runner{Service: service, Output: &recordedLines{}}
		if code := runner.Run(t.Context(), "setup", []string{"network"}); code != command.ExitFailure || service.elevation {
			t.Fatal(state, code)
		}
	}
	for _, report := range []platformsetup.Report{{InspectionError: "read failed"}, {PreferenceError: "unreadable"}} {
		runner := Runner{Service: &fakeService{report: report}, Output: &recordedLines{}}
		if code := runner.Run(t.Context(), "doctor", []string{"network"}); code != command.ExitFailure {
			t.Fatal(code)
		}
	}
	runner := Runner{Service: &fakeService{skipErr: errors.New("disk full")}, Output: &recordedLines{}}
	if code := runner.Run(t.Context(), "setup", []string{"network", "--skip"}); code != command.ExitFailure {
		t.Fatal(code)
	}
}

func TestJSONReportPreservesEvidenceOnItsOwnStream(t *testing.T) {
	var human recordedLines
	var machine bytes.Buffer
	service := &fakeService{report: platformsetup.Report{Executable: "wind.exe", Preference: platformsetup.Preference{Choice: platformsetup.Skip}, Findings: []platformsetup.Finding{{
		Network: platformsetup.Network{Interface: "Wi-Fi", Profile: platformsetup.Private}, Protocol: platformsetup.UDP,
		Status: platformsetup.PolicyRestricted, Reason: platformsetup.ExplicitBlock,
		Rules: []platformsetup.Rule{{Name: "company policy", Action: platformsetup.Block}},
	}}}}
	runner := Runner{Service: service, Output: &human, JSONOutput: &machine}
	if code := runner.Run(t.Context(), "doctor", []string{"network", "--json"}); code != command.ExitOK {
		t.Fatal(code)
	}
	var report jsonReport
	if err := json.Unmarshal(machine.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(human) != 0 || report.Choice != "skip" || report.Scope != platformsetup.InspectionScope || len(report.Findings) != 1 || report.Findings[0].SuggestSetup || len(report.Findings[0].Rules) != 1 {
		t.Fatal(human, machine.String())
	}
}

func TestCIAndRedirectedStreamsNeverAuthorizeElevation(t *testing.T) {
	t.Setenv("CI", "true")
	if IsInteractive(&bytes.Buffer{}, &bytes.Buffer{}) {
		t.Fatal("CI was interactive")
	}
	t.Setenv("CI", "false")
	if IsInteractive(&bytes.Buffer{}, &bytes.Buffer{}) {
		t.Fatal("redirected streams were interactive")
	}
}

func TestNetworkCommandReportsActionableParseDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"unknown target", "doctor", []string{"firewall"}, `doctor: unknown target "firewall"`},
		{"unknown option", "doctor", []string{"network", "--skip"}, `doctor network: unknown option "--skip"`},
		{"unexpected positional", "doctor", []string{"network", "extra"}, `doctor network: unexpected positional argument "extra"`},
		{"conflicting setup flags", "setup", []string{"network", "--skip", "--remove"}, "setup network: --skip and --remove cannot be used together"},
		{"invalid json flag", "doctor", []string{"network", "--json=maybe"}, `doctor network: invalid value "maybe" for --json: parse error`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var lines recordedLines
			runner := Runner{Service: &fakeService{}, Output: &lines}
			if code := runner.Run(t.Context(), tc.cmd, tc.args); code != command.ExitUsage {
				t.Fatalf("code=%d lines=%q", code, lines)
			}
			joined := strings.Join(lines, "\n")
			if !strings.Contains(joined, tc.want) || !strings.Contains(joined, "Usage:") {
				t.Fatalf("output=%q want=%q", joined, tc.want)
			}
		})
	}
}
