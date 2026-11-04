package networkcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/internal/platformsetup"
)

type fakeService struct {
	checks, setups, skips int
	elevation             bool
	action                platformsetup.RuleAction
	report                platformsetup.Report
	operation             platformsetup.Operation
	skipErr               error
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
func (service *fakeService) Skip() error { service.skips++; return service.skipErr }

func TestCommandsKeepReadingPreferenceAndMutationIndependent(t *testing.T) {
	for _, test := range []struct {
		command               string
		args                  []string
		checks, setups, skips int
		elevation             bool
		action                platformsetup.RuleAction
		code                  int
	}{
		{"doctor", []string{"network"}, 1, 0, 0, false, 0, ResultOK},
		{"doctor", []string{"network", "--json"}, 1, 0, 0, false, 0, ResultOK},
		{"setup", []string{"network"}, 0, 1, 0, true, platformsetup.EnsureRules, ResultOK},
		{"setup", []string{"network", "--non-interactive"}, 0, 1, 0, false, platformsetup.EnsureRules, ResultOK},
		{"setup", []string{"network", "--json"}, 0, 1, 0, false, platformsetup.EnsureRules, ResultOK},
		{"setup", []string{"network", "--remove"}, 0, 1, 0, true, platformsetup.RemoveRules, ResultOK},
		{"setup", []string{"network", "--skip"}, 0, 0, 1, false, 0, ResultOK},
		{"setup", []string{"network", "--skip", "--json"}, 0, 0, 1, false, 0, ResultOK},
		{"setup", []string{"network", "--skip", "--remove"}, 0, 0, 0, false, 0, ResultUsage},
		{"doctor", []string{"network", "--skip"}, 0, 0, 0, false, 0, ResultUsage},
		{"doctor", []string{"network", "extra"}, 0, 0, 0, false, 0, ResultUsage},
		{"setup", []string{"network", "--help"}, 0, 0, 0, false, 0, ResultOK},
		{"setup", nil, 0, 0, 0, false, 0, ResultUsage},
	} {
		service := &fakeService{operation: platformsetup.Operation{State: platformsetup.RulesApplied}}
		runner := Runner{Service: service, Output: &bytes.Buffer{}, Interactive: func() bool { return true }}
		code := runner.Run(t.Context(), test.command, test.args)
		if code != test.code || service.checks != test.checks || service.setups != test.setups || service.skips != test.skips || service.elevation != test.elevation || service.action != test.action {
			t.Fatalf("%s %v: code=%d, service=%+v", test.command, test.args, code, service)
		}
	}
}

func TestCommandFailuresRemainSeparateFromPolicyVerdicts(t *testing.T) {
	for _, state := range []platformsetup.OperationState{platformsetup.AuthorizationDeclined, platformsetup.PermissionRequired, platformsetup.OperationFailed, platformsetup.OperationUnsupported} {
		service := &fakeService{operation: platformsetup.Operation{State: state, Detail: "failure"}}
		runner := Runner{Service: service, Output: &bytes.Buffer{}}
		if code := runner.Run(t.Context(), "setup", []string{"network"}); code != ResultFailure || service.elevation {
			t.Fatal(state, code)
		}
	}
	for _, report := range []platformsetup.Report{{InspectionError: "read failed"}, {PreferenceError: "unreadable"}} {
		runner := Runner{Service: &fakeService{report: report}, Output: &bytes.Buffer{}}
		if code := runner.Run(t.Context(), "doctor", []string{"network"}); code != ResultFailure {
			t.Fatal(code)
		}
	}
	runner := Runner{Service: &fakeService{skipErr: errors.New("disk full")}, Output: &bytes.Buffer{}}
	if code := runner.Run(t.Context(), "setup", []string{"network", "--skip"}); code != ResultFailure {
		t.Fatal(code)
	}
}

func TestJSONReportPreservesEvidenceOnItsOwnStream(t *testing.T) {
	var human, machine bytes.Buffer
	service := &fakeService{report: platformsetup.Report{Executable: "wind.exe", Preference: platformsetup.Preference{Choice: platformsetup.Skip}, Findings: []platformsetup.Finding{{
		Network: platformsetup.Network{Interface: "Wi-Fi", Profile: platformsetup.Private}, Protocol: platformsetup.UDP,
		Status: platformsetup.PolicyRestricted, Reason: platformsetup.ExplicitBlock,
		Rules: []platformsetup.Rule{{Name: "company policy", Action: platformsetup.Block}},
	}}}}
	runner := Runner{Service: service, Output: &human, JSONOutput: &machine}
	if code := runner.Run(t.Context(), "doctor", []string{"network", "--json"}); code != ResultOK {
		t.Fatal(code)
	}
	var report jsonReport
	if err := json.Unmarshal(machine.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if human.Len() != 0 || report.Choice != "skip" || report.Scope != platformsetup.InspectionScope || len(report.Findings) != 1 || report.Findings[0].SuggestSetup || len(report.Findings[0].Rules) != 1 {
		t.Fatal(human.String(), machine.String())
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
