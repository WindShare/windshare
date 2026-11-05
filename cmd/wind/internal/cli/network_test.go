package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/liveshare"
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

func TestReceiveNeverInspectsFirewall(t *testing.T) {
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
			want := int32(0)
			if got := platform.checks.Load(); got != want {
				t.Fatal("policy check count", got, want)
			}
		})
	}
}

func TestShareNeverInspectsFirewall(t *testing.T) {
	platform := &commandNetworkPlatform{}
	var prepared atomic.Bool
	app := &App{Stderr: &bytes.Buffer{}, networkSetup: commandNetworkService(t, platform),
		engineConfig: engine.Config{Share: engine.ShareDependencies{
			Prepare: func(context.Context, liveshare.SenderConfig) (engine.PreparedShare, error) {
				prepared.Store(true)
				return nil, errors.New("injected preparation failure before dialing")
			},
		}}}
	if code := app.Run(t.Context(), []string{"share", t.TempDir(), "--relay", "https://relay.example", "--front-url", "https://front.example"}); code != ExitUsage {
		t.Fatal(code)
	}
	if !prepared.Load() || platform.checks.Load() != 0 {
		t.Fatal("share read firewall policy or never started")
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
	if err != nil || choice.Choice != platformsetup.Skip {
		t.Fatal(choice, err)
	}
}

type renderedNetworkPlatform struct{}

func (renderedNetworkPlatform) Inspect(context.Context, string) (platformsetup.Snapshot, error) {
	return platformsetup.Snapshot{
		Networks: []platformsetup.Network{{Interface: "Ethernet", Profile: platformsetup.Private}},
		Profiles: []platformsetup.ProfilePolicy{{Profile: platformsetup.Private, Enabled: platformsetup.Yes,
			AllowInboundRules: platformsetup.Yes, AllowLocalRules: platformsetup.Yes, DefaultInbound: platformsetup.Block}},
		Rules: []platformsetup.Rule{{Name: "WindShare UDP", Profiles: []platformsetup.Profile{platformsetup.Private},
			Protocol: platformsetup.UDP, Action: platformsetup.Allow, Conditional: true, Restrictions: "RemotePort=5000"}},
	}, nil
}
func (renderedNetworkPlatform) Apply(context.Context, string, platformsetup.RuleAction, bool) platformsetup.Operation {
	return platformsetup.Operation{State: platformsetup.PermissionRequired, Detail: "run as administrator"}
}

// Stderr is a complete-line canvas, so every human network line must reach it
// in one write instead of being split at each formatted fragment.
func TestNetworkCommandsKeepHumanLinesWhole(t *testing.T) {
	for _, test := range []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"setup", "network", "--non-interactive"}, ExitFailure, []string{
			"Rule operation: permission-required (run as administrator)",
			"Ethernet private UDP: rule-restricted (conditional-rule)",
			"  WindShare UDP: allow (RemotePort=5000)",
		}},
		{[]string{"setup", "network", "--unknown"}, ExitUsage, []string{
			"Usage: wind setup network [--json] [--skip|--remove] [--non-interactive]",
		}},
	} {
		stderr := &bytes.Buffer{}
		app := &App{Stdout: &bytes.Buffer{}, Stderr: stderr, networkSetup: &platformsetup.Service{Platform: renderedNetworkPlatform{},
			Preferences: platformsetup.FilePreferences{Path: filepath.Join(t.TempDir(), "preference.json")},
			Executable:  func() (string, error) { return "wind.exe", nil }}}
		if code := app.Run(t.Context(), test.args); code != test.code {
			t.Fatal(test.args, code)
		}
		lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, " (") || strings.HasPrefix(line, " [") {
				t.Fatalf("%v rendered a split line %q in:\n%s", test.args, line, stderr)
			}
		}
		for _, want := range test.want {
			if !slices.Contains(lines, want) {
				t.Fatalf("%v missing line %q in:\n%s", test.args, want, stderr)
			}
		}
	}
}
