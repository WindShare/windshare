package platformsetup

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSetupClassifiesUnsupportedExecutable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		err    error
		want   OperationState
		action RuleAction
	}{
		{"ensure unsupported", ErrUnsupported, OperationUnsupported, EnsureRules},
		{"remove unsupported", ErrUnsupported, OperationUnsupported, RemoveRules},
		{"ensure other failure", errors.New("path vanished"), OperationFailed, EnsureRules},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			platformCalls := 0
			service := Service{
				Preferences: FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)},
				Executable:  func() (string, error) { return "", test.err },
				Platform: fakePlatform{
					apply: func(context.Context, string, RuleAction, bool) Operation {
						platformCalls++
						return Operation{State: RulesApplied}
					},
				},
			}
			operation, report := service.Setup(t.Context(), test.action, false)
			if operation.State != test.want || operation.Detail != test.err.Error() {
				t.Fatalf("Setup = %+v, want state=%q detail=%q", operation, test.want, test.err.Error())
			}
			if platformCalls != 0 {
				t.Fatalf("platform Apply called %d times after Executable failed", platformCalls)
			}
			if test.want == OperationUnsupported {
				if report.InspectionError != ErrUnsupported.Error() || report.SuggestSetup() {
					t.Fatalf("unsupported Setup report=%+v", report)
				}
			}
		})
	}
}

func TestSetupOnUnsupportedPlatformMatchesCheckVocabulary(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("installationExecutable is supported on Windows")
	}
	service := New()
	// A saved skip choice legitimately takes precedence over platform support at startup.
	service.Preferences = FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
	operation, report := service.Setup(t.Context(), EnsureRules, true)
	if operation.State != OperationUnsupported {
		t.Fatalf("New().Setup state=%q detail=%q, want unsupported", operation.State, operation.Detail)
	}
	check := service.Check(t.Context())
	if report.InspectionError != check.InspectionError || report.InspectionError != ErrUnsupported.Error() {
		t.Fatalf("Setup report inspection=%q check=%q", report.InspectionError, check.InspectionError)
	}
	if startup := service.Startup(); startup.Decision != SetupUnsupported {
		t.Fatalf("Startup decision=%v, want unsupported", startup.Decision)
	}
}

func TestSupportsSetupMatchesExecutableGate(t *testing.T) {
	t.Parallel()
	service := &Service{
		Preferences: FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)},
		Executable:  func() (string, error) { return "", ErrUnsupported },
	}
	if service.SupportsSetup() {
		t.Fatal("unsupported executable still reported setup support")
	}
	service.Executable = func() (string, error) { return `C:\wind.exe`, nil }
	if !service.SupportsSetup() {
		t.Fatal("resolvable executable reported no setup support")
	}
	service.Executable = func() (string, error) { return "", errors.New("path missing") }
	if !service.SupportsSetup() {
		t.Fatal("non-unsupported executable errors must not look like an unsupported platform")
	}
}
