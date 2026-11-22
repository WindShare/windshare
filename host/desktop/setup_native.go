package desktop

import (
	"context"
	"errors"
	"os"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

type relaySetupEngine interface {
	StartRelaySetup(context.Context, engine.RelaySetupRequest) (*engine.Task[engine.RelaySetupResult], error)
	CancelRelaySetup(context.Context, string) error
}

type nativeSetup struct {
	settings relayconfig.FileStore
	engine   relaySetupEngine
	hostname func() (string, error)
}

func newNativeSetup(settings relayconfig.FileStore, app relaySetupEngine) nativeSetup {
	return nativeSetup{settings: settings, engine: app, hostname: os.Hostname}
}

func (setup nativeSetup) defaultDeviceName() string {
	var host string
	if setup.hostname != nil {
		host, _ = setup.hostname()
	}
	return relaysetup.DefaultDeviceName(host)
}

func (setup nativeSetup) run(ctx context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
	var result SetupResult
	if input.Preference != nil {
		_, err := setup.settings.Update(ctx, input.Preference.Apply)
		result.FrontendSaved = err == nil && input.Preference.Action == "frontend"
		return result, err
	}
	if input.FrontURL != "" {
		if _, err := setup.settings.Update(ctx, func(config *relayconfig.Config) error {
			config.FrontURL = input.FrontURL
			return nil
		}); err != nil {
			return result, err
		}
		// The explicit frontend edit has its own commit point. Relay failure or
		// cancellation cannot roll it back over a later CLI or GUI edit.
		result.FrontendSaved = true
		if progress != nil {
			progress(relaysetup.Progress{Stage: "frontend_saved"})
		}
	}
	if input.RelayURL == "" {
		return result, nil
	}
	current, err := setup.engine.StartRelaySetup(ctx, engine.RelaySetupRequest{
		URL: input.RelayURL, Operation: input.RelayOperation, AccessKey: input.AccessKey,
		DeviceName: setup.defaultDeviceName(),
	})
	if err != nil {
		return result, err
	}
	for observation := range current.Observations() {
		if event, ok := observation.Event.(engine.RelaySetupProgress); ok && progress != nil {
			progress(event)
		}
	}
	settled, err := current.Wait(context.Background())
	if err == nil {
		err = settled.Err
	}
	if err == nil {
		result.RelayURL, result.RelaySaved = settled.Value.RelayURL, true
	} else {
		var failure *relaysetup.SetupError
		if errors.As(err, &failure) && failure.Recovery.Action != relaysetup.RecoveryBlocked {
			result.RelayURL = failure.Recovery.RelayURL
		}
	}
	return result, err
}

func (setup nativeSetup) cancel(ctx context.Context, address string) error {
	if address == "" {
		return nil
	}
	err := setup.engine.CancelRelaySetup(ctx, address)
	// Run has joined before this call. An installation that won the race stays
	// committed; only an unfinished enrollment belongs to this cleanup.
	if errors.Is(err, relaysetup.ErrNoPendingSetup) {
		return nil
	}
	return err
}
