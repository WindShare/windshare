package platformsetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakePlatform struct {
	inspect func(context.Context, string) (Snapshot, error)
	apply   func(context.Context, string, RuleAction, bool) Operation
}

func (p fakePlatform) Inspect(ctx context.Context, executable string) (Snapshot, error) {
	return p.inspect(ctx, executable)
}
func (p fakePlatform) Apply(ctx context.Context, executable string, action RuleAction, elevation bool) Operation {
	return p.apply(ctx, executable, action, elevation)
}

func TestPreferencesBindOnlyCompletedSetupToAnExecutable(t *testing.T) {
	store := FilePreferences{Path: filepath.Join(t.TempDir(), "preferences", PreferenceFileName)}
	if preference, err := store.Load(); preference != (Preference{}) || err != nil {
		t.Fatal(preference, err)
	}
	if _, err := os.Stat(filepath.Dir(store.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created preference directory", err)
	}
	for _, preference := range []Preference{{}, {Choice: Skip}, {Choice: Configure, Executable: "wind.exe"}, {Choice: Configure, Executable: "C:\\?\\wind\x7f.exe"}} {
		if err := store.Save(preference); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Load(); got != preference || err != nil {
			t.Fatal(got, err)
		}
	}
	for _, data := range []string{`{`, `{"schema":0,"choice":"skip"}`, `{"schema":2,"choice":"other"}`,
		`{"schema":2,"choice":"configure"}`, `{"schema":2,"choice":"skip","executable":"wind.exe"}`} {
		if err := os.WriteFile(store.Path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("invalid preference accepted", data)
		}
	}
	for _, preference := range []Preference{{Choice: Choice(255)}, {Choice: Configure}, {Choice: Ask, Executable: "wind.exe"}} {
		if err := store.Save(preference); err == nil {
			t.Fatal("invalid preference saved", preference)
		}
	}
	t.Setenv(PreferenceEnvironment, store.Path)
	if err := (FilePreferences{}).Save(Preference{Choice: Skip}); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorAlwaysInspectsDespiteInstallationHistory(t *testing.T) {
	store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
	reads := 0
	service := Service{Preferences: store, Executable: func() (string, error) { return "wind.exe", nil },
		Platform: fakePlatform{inspect: func(ctx context.Context, path string) (Snapshot, error) {
			reads++
			if _, ok := ctx.Deadline(); !ok || path != "wind.exe" {
				t.Fatal("invalid inspection scope")
			}
			return defaultSnapshot(), nil
		}}}
	for _, preference := range []Preference{{Choice: Skip}, {Choice: Configure, Executable: "wind.exe"}} {
		if err := store.Save(preference); err != nil {
			t.Fatal(err)
		}
		report := service.Check(t.Context())
		if report.Preference != preference || !report.SuggestSetup() {
			t.Fatal("history became policy evidence", report)
		}
	}
	if reads != 2 {
		t.Fatal("diagnostic skipped live inspection", reads)
	}
}

func TestSetupPersistsOnlySuccessfulWritesAndAlwaysRechecks(t *testing.T) {
	for _, state := range []OperationState{RulesApplied, AuthorizationDeclined, PermissionRequired, OperationFailed, RulesRemoved} {
		t.Run(string(state), func(t *testing.T) {
			store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
			if err := store.Save(Preference{Choice: Skip}); err != nil {
				t.Fatal(err)
			}
			reads := 0
			service := Service{Preferences: store, Executable: func() (string, error) { return "wind.exe", nil }, Platform: fakePlatform{
				apply: func(_ context.Context, path string, action RuleAction, elevation bool) Operation {
					if path != "wind.exe" || action != EnsureRules || !elevation {
						t.Fatal("incorrect write scope")
					}
					return Operation{State: state}
				},
				inspect: func(context.Context, string) (Snapshot, error) { reads++; return defaultSnapshot(), nil },
			}}
			operation, report := service.Setup(t.Context(), EnsureRules, true)
			if operation.State != state || reads != 1 || !report.SuggestSetup() {
				t.Fatal(operation, report, reads)
			}
			want := Preference{Choice: Skip}
			if state == RulesApplied {
				want = Preference{Choice: Configure, Executable: "wind.exe"}
			}
			if report.Preference != want {
				t.Fatal("unexpected saved choice", report)
			}
		})
	}
}

func TestRemoveForgetsOnlyMatchingInstallation(t *testing.T) {
	for _, preference := range []Preference{{Choice: Configure, Executable: "WIND.exe"}, {Choice: Configure, Executable: "other.exe"}, {Choice: Skip}} {
		store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
		if err := store.Save(preference); err != nil {
			t.Fatal(err)
		}
		service := Service{Preferences: store, Executable: func() (string, error) { return "wind.exe", nil }, Platform: fakePlatform{
			apply:   func(context.Context, string, RuleAction, bool) Operation { return Operation{State: RulesRemoved} },
			inspect: func(context.Context, string) (Snapshot, error) { return defaultSnapshot(), nil },
		}}
		_, report := service.Setup(t.Context(), RemoveRules, false)
		want := preference
		if preference.Executable == "WIND.exe" {
			want = Preference{}
		}
		if report.Preference != want {
			t.Fatal(report, want)
		}
	}
}

func TestSkipCannotReadOrMutateSystemPolicy(t *testing.T) {
	store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
	service := Service{Preferences: store}
	if err := service.Skip(); err != nil {
		t.Fatal(err)
	}
	if preference, err := store.Load(); err != nil || preference.Choice != Skip {
		t.Fatal(preference, err)
	}
}

func TestReadFailureCannotOfferRepair(t *testing.T) {
	for _, err := range []error{errors.New("denied"), context.DeadlineExceeded, ErrUnsupported} {
		service := Service{Preferences: FilePreferences{Path: filepath.Join(t.TempDir(), "missing")},
			Executable: func() (string, error) { return "wind.exe", nil },
			Platform:   fakePlatform{inspect: func(context.Context, string) (Snapshot, error) { return Snapshot{}, err }}}
		report := service.Check(t.Context())
		if report.InspectionError == "" || report.SuggestSetup() || report.Findings[0].Status != UnknownStatus {
			t.Fatal(report)
		}
	}
}
