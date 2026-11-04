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

func TestPreferencesAreUserChoicesAndNeverPolicyEvidence(t *testing.T) {
	store := FilePreferences{Path: filepath.Join(t.TempDir(), "preferences", PreferenceFileName)}
	if choice, err := store.Load(); choice != Ask || err != nil {
		t.Fatal(choice, err)
	}
	if _, err := os.Stat(filepath.Dir(store.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created preference directory", err)
	}
	for _, choice := range []Choice{Skip, Configure, Ask} {
		if err := store.Save(choice); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Load(); got != choice || err != nil {
			t.Fatal(got, err)
		}
	}
	for _, data := range []string{`{`, `{"schema":0,"choice":"skip"}`, `{"schema":1,"choice":"configured"}`} {
		if err := os.WriteFile(store.Path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("invalid preference accepted", data)
		}
	}
	if err := store.Save(Choice(255)); err == nil {
		t.Fatal("invalid choice saved")
	}
	t.Setenv(PreferenceEnvironment, store.Path)
	if err := (FilePreferences{}).Save(Skip); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{"first/wind.exe", "moved/wind.exe"} {
		service := Service{Preferences: store, Executable: func() (string, error) { return executable, nil },
			Platform: fakePlatform{inspect: func(context.Context, string) (Snapshot, error) { return defaultSnapshot(), nil }}}
		report := service.Check(t.Context())
		if report.Choice != Skip || !report.Repairable() {
			t.Fatal("choice affected policy", report)
		}
	}
}

func TestServiceRechecksAfterEveryRuleOutcome(t *testing.T) {
	for _, state := range []OperationState{RulesApplied, AuthorizationDeclined, PermissionRequired, OperationFailed, RulesRemoved} {
		t.Run(string(state), func(t *testing.T) {
			store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
			reads := 0
			service := Service{Preferences: store, Executable: func() (string, error) { return "wind.exe", nil }, Platform: fakePlatform{
				apply: func(_ context.Context, path string, action RuleAction, elevation bool) Operation {
					if path != "wind.exe" || action != EnsureRules || !elevation {
						t.Fatal(path, action, elevation)
					}
					return Operation{State: state}
				},
				inspect: func(ctx context.Context, _ string) (Snapshot, error) {
					reads++
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("unbounded policy check")
					}
					return defaultSnapshot(), nil
				},
			}}
			operation, report := service.Setup(t.Context(), EnsureRules, true)
			if operation.State != state || reads != 1 || !report.Repairable() {
				t.Fatal(operation, report, reads)
			}
			want := Ask
			if state == RulesApplied {
				want = Configure
			}
			if report.Choice != want {
				t.Fatal("operation changed preference unexpectedly", report)
			}
		})
	}
}

func TestSkipCannotReadOrMutateSystemPolicy(t *testing.T) {
	store := FilePreferences{Path: filepath.Join(t.TempDir(), PreferenceFileName)}
	service := Service{Preferences: store}
	if err := service.Skip(); err != nil {
		t.Fatal(err)
	}
	if choice, err := store.Load(); choice != Skip || err != nil {
		t.Fatal(choice, err)
	}
}

func TestReadFailureCannotOfferRepair(t *testing.T) {
	for _, err := range []error{errors.New("denied"), context.DeadlineExceeded, ErrUnsupported} {
		service := Service{Preferences: FilePreferences{Path: filepath.Join(t.TempDir(), "missing")},
			Executable: func() (string, error) { return "wind.exe", nil },
			Platform:   fakePlatform{inspect: func(context.Context, string) (Snapshot, error) { return Snapshot{}, err }}}
		report := service.Check(t.Context())
		if report.PolicyError == "" || report.Repairable() || report.Findings[0].Verdict != Unknown {
			t.Fatal(report)
		}
	}
}
