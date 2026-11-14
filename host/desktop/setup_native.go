package desktop

import (
	"context"
	"errors"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

type SetupResult struct {
	FrontendSaved bool
	RelayURL      string
	RelaySaved    bool
}

type nativeSetup struct {
	settings relayconfig.FileStore
	service  *relaysetup.Service
}

func newNativeSetup(settings relayconfig.FileStore) nativeSetup {
	return nativeSetup{settings: settings, service: relaysetup.New(relaysetup.Dependencies{
		Store: relaysetup.FileStore{Settings: settings},
	})}
}

func (setup nativeSetup) run(ctx context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
	var result SetupResult
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
	installed, err := setup.service.Run(ctx, relaysetup.Request{
		URL: input.RelayURL, Operation: relaysetup.AddProfile,
		DeviceName: relaysetup.DefaultDeviceName("WindShare desktop"), NoBrowser: true,
	}, progress)
	if err == nil {
		result.RelayURL, result.RelaySaved = installed.RelayURL, true
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
	err := setup.service.Cancel(ctx, address)
	// Run has joined before this call. An installation that won the race stays
	// committed; only an unfinished enrollment belongs to this cleanup.
	if errors.Is(err, relaysetup.ErrNoPendingSetup) {
		return nil
	}
	return err
}
