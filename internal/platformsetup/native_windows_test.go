package platformsetup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsAdapterSeparatesElevationAndRuleOutcomes(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "wind.exe")
	if err := os.WriteFile(executable, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name           string
		admin, consent bool
		action         RuleAction
		output         string
		runErr         error
		want           OperationState
	}{
		{"unattended standard user", false, false, EnsureRules, "", nil, PermissionRequired},
		{"unattended administrator", true, false, EnsureRules, `{"Code":0}`, nil, RulesApplied},
		{"interactive standard user", false, true, EnsureRules, `{"Code":0}`, nil, RulesApplied},
		{"UAC cancel", false, true, EnsureRules, `{"Code":1223}`, nil, AuthorizationDeclined},
		{"access denied", true, false, EnsureRules, `{"Code":5}`, errors.New("exit 5"), PermissionRequired},
		{"ownership conflict", true, false, EnsureRules, `{"Code":10}`, errors.New("exit 10"), OperationFailed},
		{"writer failure", true, false, EnsureRules, `{"Code":1,"Detail":"policy denied"}`, errors.New("exit 1"), OperationFailed},
		{"remove", true, false, RemoveRules, `{"Code":0}`, nil, RulesRemoved},
		{"bad response", true, false, EnsureRules, `bad json`, nil, OperationFailed},
		{"missing result", true, false, EnsureRules, `{}`, nil, OperationFailed},
		{"failed process with success output", true, false, EnsureRules, `{"Code":0}`, errors.New("process failure"), OperationFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			runs := 0
			platform := windowsPlatform{isElevated: func() bool { return test.admin }, run: func(_ context.Context, script string) ([]byte, error) {
				runs++
				elevated := strings.Contains(script, "$RulesScript = ")
				if elevated != (!test.admin && test.consent) {
					t.Fatal("unexpected elevation")
				}
				if len(encodePowerShell(script)) > 30000 {
					t.Fatal("script exceeds Windows command line capacity")
				}
				return []byte(test.output), test.runErr
			}}
			got := platform.Apply(t.Context(), executable, test.action, test.consent)
			if got.State != test.want || runs == 0 && (test.admin || test.consent) {
				t.Fatal(got, runs)
			}
		})
	}
	platform := nativePlatform()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := platform.Apply(ctx, executable, EnsureRules, true); got.State != OperationFailed {
		t.Fatal(got)
	}
	if got := platform.Apply(t.Context(), executable, RuleAction(255), false); got.State != OperationFailed {
		t.Fatal(got)
	}
	if got := platform.Apply(t.Context(), "missing.exe", EnsureRules, false); got.State != OperationFailed {
		t.Fatal(got)
	}
}

func TestWindowsAdapterRejectsUnreadablePolicy(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{`bad json`, `{}`, `{"Networks":[],"Profiles":[],"Rules":[]}`} {
		platform := windowsPlatform{run: func(context.Context, string) ([]byte, error) { return []byte(output), nil }}
		_, err := platform.Inspect(t.Context(), executable)
		if (err != nil) != (output == "bad json" || output == "{}") {
			t.Fatal(output, err)
		}
	}
	platform := windowsPlatform{run: func(context.Context, string) ([]byte, error) { return nil, errors.New("read denied") }}
	if _, err := platform.Inspect(t.Context(), executable); err == nil {
		t.Fatal("read failure discarded")
	}
	if _, err := platform.Inspect(t.Context(), "missing.exe"); err == nil {
		t.Fatal("path failure discarded")
	}
}

func TestElevationWrapperPropagatesCancellation(t *testing.T) {
	for _, test := range []struct {
		prefix string
		code   int
	}{
		{`function Start-Process { throw [ComponentModel.Win32Exception]::new(1223) }`, 1223},
		{`function Start-Process { throw 'start failed' }`, 1},
	} {
		output, err := runPowerShell(t.Context(), test.prefix+"\n$RulesScript = 'test-rules'\n"+elevateScript)
		var result struct{ Code int }
		if err != nil || json.Unmarshal(output, &result) != nil || result.Code != test.code {
			t.Fatal(string(output), err)
		}
	}
}

func TestElevatedResultPipePreservesRuleFailureDetails(t *testing.T) {
	// The real parent/child pipe is exercised, but this launcher strips RunAs and
	// every child firewall cmdlet is replaced with the fake rule implementation.
	const launcher = `function Start-Process {
    param($FilePath, $Verb, $WindowStyle, [switch]$PassThru, $ArgumentList)
    if ($Verb -ne 'RunAs' -or $WindowStyle -ne 'Hidden') { throw 'Bad elevation scope.' }
    Microsoft.PowerShell.Management\Start-Process -FilePath $FilePath -WindowStyle Hidden -PassThru -ArgumentList $ArgumentList
}`
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	child := "$CallLog = " + quotePowerShell(log) + "\n$Scenario = 'failure'\n" + fakeRuleCommands + ruleScript(`C:\wind.exe`, EnsureRules)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := runPowerShell(ctx, launcher+"\n$RulesScript = "+quotePowerShell(child)+"\n"+elevateScript)
	var result struct {
		Code   int
		Detail string
	}
	if err != nil || json.Unmarshal(output, &result) != nil || result.Code != 1 || result.Detail != "Injected rule write failure." {
		t.Fatal(string(output), err)
	}
}
