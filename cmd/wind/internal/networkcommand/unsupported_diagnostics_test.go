package networkcommand

import (
	"bytes"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/platformsetup"
)

func TestHumanFindingsOmitEmptyInterfacePrefix(t *testing.T) {
	var human recordedLines
	service := &fakeService{report: platformsetup.Report{
		InspectionError: platformsetup.ErrUnsupported.Error(),
		Findings: []platformsetup.Finding{
			{Protocol: platformsetup.UDP, Reason: platformsetup.UnsupportedPlatform},
			{Protocol: platformsetup.TCP, Reason: platformsetup.UnsupportedPlatform},
		},
	}}
	runner := Runner{Service: service, Output: &human}
	if code := runner.Run(t.Context(), "doctor", []string{"network"}); code != ResultFailure {
		t.Fatalf("code=%d human=%q", code, human)
	}
	joined := strings.Join(human, "\n")
	for _, line := range human {
		if strings.Contains(line, "UDP:") || strings.Contains(line, "TCP:") {
			if strings.HasPrefix(line, " ") {
				t.Fatalf("finding line has a leading space: %q\nfull:\n%s", line, joined)
			}
			if strings.Contains(line, "  ") {
				t.Fatalf("finding line has doubled spaces: %q", line)
			}
		}
	}
	if !strings.Contains(joined, "UDP: unknown (unsupported-platform)") {
		t.Fatalf("missing UDP finding:\n%s", joined)
	}
	if !strings.Contains(joined, "TCP: unknown (unsupported-platform)") {
		t.Fatalf("missing TCP finding:\n%s", joined)
	}
}

func TestHumanFindingsKeepInterfaceAndProfileWhenPresent(t *testing.T) {
	var human recordedLines
	service := &fakeService{report: platformsetup.Report{Findings: []platformsetup.Finding{{
		Network:  platformsetup.Network{Interface: "Wi-Fi", Profile: platformsetup.Private},
		Protocol: platformsetup.UDP, Status: platformsetup.RuleMissing, Reason: platformsetup.NoMatchingRule,
	}}}}
	runner := Runner{Service: service, Output: &human}
	if code := runner.Run(t.Context(), "doctor", []string{"network"}); code != ResultOK {
		t.Fatalf("code=%d", code)
	}
	joined := strings.Join(human, "\n")
	if !strings.Contains(joined, "Wi-Fi private UDP: rule-missing (no-matching-rule)") {
		t.Fatalf("interface finding changed:\n%s", joined)
	}
}

func TestSetupOmitsWindowsPreambleWhenUnsupported(t *testing.T) {
	var human recordedLines
	service := &fakeService{
		operation: platformsetup.Operation{
			State:  platformsetup.OperationUnsupported,
			Detail: platformsetup.ErrUnsupported.Error(),
		},
		report: platformsetup.Report{
			InspectionError: platformsetup.ErrUnsupported.Error(),
			Findings: []platformsetup.Finding{
				{Protocol: platformsetup.UDP, Reason: platformsetup.UnsupportedPlatform},
			},
		},
		supportsSetup: false,
	}
	runner := Runner{Service: service, Output: &human, Interactive: func() bool { return true }}
	if code := runner.Run(t.Context(), "setup", []string{"network"}); code != ResultFailure {
		t.Fatalf("code=%d human=%q", code, human)
	}
	joined := strings.Join(human, "\n")
	if strings.Contains(joined, "Allow inbound UDP/TCP") {
		t.Fatalf("unsupported setup still printed Windows consent preamble:\n%s", joined)
	}
	if strings.Contains(joined, "Remove this executable") {
		t.Fatalf("unsupported remove still printed Windows consent preamble:\n%s", joined)
	}
	if !strings.Contains(joined, "Rule operation: unsupported") {
		t.Fatalf("missing unsupported operation:\n%s", joined)
	}
}

func TestSetupStillPrintsPreambleWhenSupported(t *testing.T) {
	var human recordedLines
	service := &fakeService{
		operation:     platformsetup.Operation{State: platformsetup.RulesApplied},
		supportsSetup: true,
	}
	runner := Runner{Service: service, Output: &human, Interactive: func() bool { return true }}
	if code := runner.Run(t.Context(), "setup", []string{"network"}); code != ResultOK {
		t.Fatalf("code=%d", code)
	}
	joined := strings.Join(human, "\n")
	if !strings.Contains(joined, "Allow inbound UDP/TCP for this executable on domain, private, and public networks") {
		t.Fatalf("supported setup lost Windows consent preamble:\n%s", joined)
	}
}

func TestSetupJSONNeverPrintsPreamble(t *testing.T) {
	var human recordedLines
	var machine bytes.Buffer
	service := &fakeService{
		operation:     platformsetup.Operation{State: platformsetup.RulesApplied},
		supportsSetup: true,
	}
	runner := Runner{Service: service, Output: &human, JSONOutput: &machine, Interactive: func() bool { return true }}
	if code := runner.Run(t.Context(), "setup", []string{"network", "--json"}); code != ResultOK {
		t.Fatalf("code=%d", code)
	}
	if len(human) != 0 {
		t.Fatalf("json setup wrote human lines: %q", human)
	}
}
