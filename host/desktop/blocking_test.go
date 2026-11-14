package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestNetworkRetryRetainsSelectionAndUsesCurrentOwnerBackoff(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "cancel"}[cancelled], func(t *testing.T) {
			a, _, starts := fixture(t)
			start := a.deps.start
			next := &engine.NetworkUnavailableError{Reason: engine.NetworkDirectoryUnavailable, RetryAt: time.Now().Add(time.Hour)}
			a.deps.load = func() (relayconfig.Config, error) { return relayconfig.Config{}, nil }
			var attempts atomic.Int32
			a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
				attempts.Add(1)
				if next != nil {
					return nil, next
				}
				return start(ctx, request)
			}
			a.execute(createRequest())
			view := awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
			a.work.Wait()
			if view.Blocking == nil || view.Blocking.Recovery != RetryCreation || view.Blocking.RetryAtMS != next.RetryAt.UnixMilli() || a.snapshot().active() {
				t.Fatalf("waiting contract: %+v", view)
			}
			next = &engine.NetworkUnavailableError{Reason: next.Reason, RetryAt: next.RetryAt.Add(time.Minute)}
			reply := a.execute(Request{Command: "resume", OperationID: "operation"})
			if reply.Error != nil {
				t.Fatal(reply.Error)
			}
			view = awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
			a.work.Wait()
			if view.Blocking.RetryAtMS != next.RetryAt.UnixMilli() || attempts.Load() != 2 || starts.Load() != 0 {
				t.Fatal("retry did not query the current owner decision", view)
			}
			if cancelled {
				a.execute(Request{Command: "cancel", OperationID: "operation"})
			}
			next = nil
			a.execute(Request{Command: "resume", OperationID: "operation"})
			a.work.Wait()
			if cancelled {
				if starts.Load() != 0 || a.snapshot().Shares[0].Result.Outcome != OutcomeCancelled {
					t.Fatal("retry resurrected cancelled selection")
				}
			} else {
				if starts.Load() != 1 || admitted(t, a).OperationID != "operation" || len(a.operations["operation"].paths) != 1 {
					t.Fatal("retry lost selection or operation identity")
				}
			}
		})
	}
}

func TestNetworkRetryLoadsNewExplicitSettingsDuringDirectoryCooldown(t *testing.T) {
	a, _, starts := fixture(t)
	config := relayconfig.Config{}
	a.deps.load = func() (relayconfig.Config, error) { return config, nil }
	start := a.deps.start
	cooldown := &engine.NetworkUnavailableError{Reason: engine.NetworkDirectoryUnavailable, RetryAt: time.Now().Add(time.Hour)}
	a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
		candidates := request.RelayIntent.Candidates()
		if len(candidates) == 0 {
			return nil, cooldown
		}
		if len(candidates) != 1 || candidates[0].PublicURL != configured().ActiveRelays[0] || request.Frontend != configured().FrontURL {
			t.Error("retry lost the newly saved selection")
		}
		return start(ctx, request)
	}
	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
	a.work.Wait()
	config = configured()
	a.execute(Request{Command: "resume", OperationID: "operation"})
	admitted(t, a)
	if starts.Load() != 1 {
		t.Fatal("directory cooldown blocked an available configured relay")
	}
}

func TestPersistentNetworkReasonsRequireExplicitRecovery(t *testing.T) {
	for _, test := range []struct {
		reason   engine.NetworkBlockingReason
		recovery RecoveryAction
	}{
		{engine.NetworkUnprovisioned, ConfigureCreation},
		{engine.NetworkInvalidConfiguration, ReplaceBuild},
		{engine.NetworkTrustStateUnreadable, RepairTrustState},
		{engine.NetworkTrustStateInvalid, RepairTrustState},
		{engine.NetworkTrustStateUnwritable, RepairTrustState},
	} {
		t.Run(string(test.reason), func(t *testing.T) {
			a, _, _ := fixture(t)
			a.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
				return nil, &engine.NetworkUnavailableError{Reason: test.reason, Cause: errors.New("diagnostic")}
			}
			a.execute(createRequest())
			view := awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingConfiguration })
			if view.Blocking.Reason != CreationBlockingReason(test.reason) || view.Blocking.Recovery != test.recovery || view.Blocking.RetryAtMS != 0 {
				t.Fatalf("%+v", view.Blocking)
			}
		})
	}
}

func TestSetupDoesNotRequireBlockedCreation(t *testing.T) {
	for _, withShare := range []bool{false, true} {
		t.Run(map[bool]string{false: "settings", true: "running_share"}[withShare], func(t *testing.T) {
			a, _, starts := fixture(t)
			request := setupRequest()
			request.OperationID = ""
			if withShare {
				a.execute(createRequest())
				admitted(t, a)
				request.OperationID = "operation"
			}
			var saves atomic.Int32
			a.deps.setup = func(context.Context, SetupInput, func(relaysetup.Progress)) (SetupResult, error) {
				saves.Add(1)
				return SetupResult{}, nil
			}
			if reply := a.execute(request); reply.Error != nil {
				t.Fatal(reply.Error)
			}
			a.work.Wait()
			a.execute(request)
			expectedStarts := int32(0)
			if withShare {
				expectedStarts = 1
			}
			if saves.Load() != 1 || starts.Load() != expectedStarts || a.snapshot().Setup.State != "finished" {
				t.Fatal("setup replay changed existing shares")
			}
		})
	}
}

func TestMissingDefaultSettingsStillAdmitDefaultShare(t *testing.T) {
	previous, explicit := os.LookupEnv(relayconfig.PathEnvironment)
	_ = os.Unsetenv(relayconfig.PathEnvironment)
	t.Cleanup(func() {
		if explicit {
			_ = os.Setenv(relayconfig.PathEnvironment, previous)
		}
	})
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	path, err := relayconfig.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture settings exist", err)
	}
	a, _, starts := fixture(t)
	a.deps.load = relayconfig.Load
	a.execute(createRequest())
	admitted(t, a)
	if starts.Load() != 1 {
		t.Fatal("default settings required setup")
	}

	t.Setenv(relayconfig.PathEnvironment, filepath.Join(t.TempDir(), "explicit-missing.json"))
	config, err := relayconfig.Load()
	if !errors.Is(err, os.ErrNotExist) || config.FrontURL != "" {
		t.Fatal("explicit missing settings were ignored", err)
	}
}
