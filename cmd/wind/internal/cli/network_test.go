package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/platformsetup"
)

type commandNetworkPlatform struct{ checks atomic.Int32 }

func (platform *commandNetworkPlatform) Inspect(context.Context, string) (platformsetup.Snapshot, error) {
	platform.checks.Add(1)
	return platformsetup.Snapshot{}, nil
}
func (*commandNetworkPlatform) Apply(context.Context, string, platformsetup.RuleAction, bool) platformsetup.Operation {
	panic("a transfer or doctor attempted to change firewall policy")
}

func commandNetworkService(t *testing.T, platform *commandNetworkPlatform) *platformsetup.Service {
	t.Helper()
	return &platformsetup.Service{Platform: platform,
		Preferences: platformsetup.FilePreferences{Path: filepath.Join(t.TempDir(), "preference.json")},
		Executable:  func() (string, error) { return "wind.exe", nil }}
}

func TestReceiveChecksPolicyOnlyForP2PCapableModes(t *testing.T) {
	for _, mode := range []string{"auto", "p2p-only", "relay-only"} {
		t.Run(mode, func(t *testing.T) {
			platform := &commandNetworkPlatform{}
			app := &App{Stderr: &bytes.Buffer{}, networkSetup: commandNetworkService(t, platform),
				getOutputFactory: engine.OutputFactoryFunc(func(engine.OutputConfig) (engine.OutputAuthority, error) {
					return nil, errors.New("injected output failure before dialing")
				})}
			if code := app.Run(t.Context(), []string{"get", validGetTestLink(t), "--connectivity", mode}); code != ExitFailure {
				t.Fatal(code)
			}
			want := int32(1)
			if mode == "relay-only" {
				want = 0
			}
			if got := platform.checks.Load(); got != want {
				t.Fatal("policy check count", got, want)
			}
		})
	}
}

func TestDoctorAndSkipAreLocalCommands(t *testing.T) {
	platform := &commandNetworkPlatform{}
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, networkSetup: commandNetworkService(t, platform)}
	if code := app.Run(t.Context(), []string{"doctor", "network", "--json"}); code != ExitOK {
		t.Fatal(code)
	}
	if code := app.Run(t.Context(), []string{"setup", "network", "--skip"}); code != ExitOK {
		t.Fatal(code)
	}
	if platform.checks.Load() != 1 {
		t.Fatal("skip read system policy")
	}
	choice, err := app.networkSetup.Preferences.Load()
	if err != nil || choice != platformsetup.Skip {
		t.Fatal(choice, err)
	}
}
