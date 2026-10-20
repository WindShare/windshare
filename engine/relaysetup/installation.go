package relaysetup

import (
	"context"
	"fmt"
	"slices"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
)

// installation is shared by preflight and commit so the acknowledgement cannot
// bypass a constraint discovered only when publishing settings.
func installation(saved relayconfig.Config, address, accessKey, frontend string, operation Operation) (relayconfig.Config, error) {
	firstProfile := len(saved.Profiles) == 0
	candidate := senderrelay.Candidate{PublicURL: address, AccessKey: accessKey}
	if err := candidate.Validate(); err != nil {
		return relayconfig.Config{}, err
	}
	saved.Profiles = slices.Clone(saved.Profiles)
	index := slices.IndexFunc(saved.Profiles, func(profile senderrelay.Candidate) bool { return sameRelay(profile.PublicURL, address) })
	if index < 0 {
		if len(saved.Profiles) >= relayconfig.MaximumProfiles {
			return relayconfig.Config{}, fmt.Errorf("%w: limit of %d relay profiles reached; import fewer profiles before adding another relay",
				relayconfig.ErrConfig, relayconfig.MaximumProfiles)
		}
		saved.Profiles = append(saved.Profiles, candidate)
	} else {
		saved.Profiles[index] = candidate
	}
	if operation == PrivateOnboarding || (firstProfile && len(saved.ActiveRelays) == 0) {
		saved.ActiveRelays = []string{address}
	}
	if saved.FrontURL == "" {
		saved.FrontURL = frontend
	}
	saved.Pending = nil
	return saved, saved.Validate()
}

func installationResult(saved relayconfig.Config, address, credentialID string) Result {
	return Result{RelayURL: address, CredentialID: credentialID, FrontURL: saved.FrontURL, ActiveRelays: saved.ActiveRelays}
}

func (service *Service) install(ctx context.Context, revision, address, accessKey, credentialID, frontend string, operation Operation) (Result, error) {
	saved, err := service.dependencies.Store.Update(ctx, func(current *relayconfig.Config) error {
		if current.Pending != nil {
			return ErrPendingSetup
		}
		if current.SettingsRevision() != revision {
			return ErrSettingsChanged
		}
		next, err := installation(*current, address, accessKey, frontend, operation)
		if err == nil {
			*current = next
		}
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return installationResult(saved, address, credentialID), nil
}

func pendingInstallation(current relayconfig.Config, pending *relayconfig.PendingSetup) (relayconfig.Config, error) {
	if err := ownsPending(current, pending); err != nil {
		return relayconfig.Config{}, err
	}
	if current.Pending.State != relayconfig.SetupInstalling {
		return relayconfig.Config{}, ErrSetupSuperseded
	}
	if current.SettingsRevision() != pending.SettingsRevision {
		return relayconfig.Config{}, ErrSettingsChanged
	}
	return installation(current, pending.RelayURL, pending.AccessKey, pending.FrontURL, Operation(pending.Operation))
}
