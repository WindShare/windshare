package platformsetup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStartupNeverInspectsOrChangesFirewall(t *testing.T) {
	for _, test := range []struct {
		name       string
		preference Preference
		path       string
		pathErr    error
		want       StartupDecision
	}{
		{"first use", Preference{}, "wind.exe", nil, SetupUnchosen},
		{"configured", Preference{Configure, "C:\\Wind\\wind.exe"}, "c:\\wind\\WIND.exe", nil, SetupRecorded},
		{"moved", Preference{Configure, "old.exe"}, "new.exe", nil, SetupPathChanged},
		{"skip follows user", Preference{Choice: Skip}, "", errors.New("must not resolve executable"), SetupSkipped},
		{"unsupported", Preference{}, "", ErrUnsupported, SetupUnsupported},
		{"unreadable executable", Preference{}, "", errors.New("path unavailable"), SetupReadFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
			if err := store.Save(test.preference); err != nil {
				t.Fatal(err)
			}
			// No platform is installed: any inspect/apply call is a test failure.
			service := Service{Preferences: store, Executable: func() (string, error) {
				if test.preference.Choice == Skip {
					t.Fatal("skip resolved executable")
				}
				return test.path, test.pathErr
			}}
			report := service.Startup()
			if report.Decision != test.want {
				t.Fatal(report)
			}
			if report.Decision.SuggestSetup() != (test.want == SetupUnchosen || test.want == SetupPathChanged) {
				t.Fatal(report)
			}
			after, err := store.Load()
			if err != nil || after != test.preference {
				t.Fatal("startup changed saved choice", after, err)
			}
		})
	}
}

func TestUnreadablePreferenceDoesNotTriggerInspectionOrPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), PreferenceFileName)
	if err := os.WriteFile(path, []byte("invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := Service{Preferences: FilePreferences{Path: path}}
	report := service.Startup()
	if report.Decision != SetupReadFailed || report.Error == "" || report.Decision.SuggestSetup() {
		t.Fatal(report)
	}
}
