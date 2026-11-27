package relaysetup

import (
	"context"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func installationResult(saved relayconfig.Config, address, credentialID string) Result {
	result := Result{RelayURL: address, CredentialID: credentialID, FrontURL: saved.FrontURL, ActiveRelays: saved.ActiveRelays, ConfiguredOnly: saved.ConfiguredOnly}
	for _, relay := range saved.Preferences("").Relays {
		if sameRelay(relay.URL, address) {
			result.Enabled, result.ConsoleURL, result.OfferedFrontend = relay.Enabled, relay.ConsoleURL, relay.OfferedFrontend
			break
		}
	}
	return result
}

func (run *workflow) install(ctx context.Context, revision string, candidate relayprofile.Candidate, apply func(relayconfig.Config, relayprofile.Candidate) (relayconfig.Config, error)) (Result, error) {
	saved, err := run.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		if current.Pending != nil {
			return ErrPendingSetup
		}
		if current.SettingsRevision() != revision {
			return errSettingsChangedBeforePending
		}
		next, err := apply(current.Config, candidate)
		if err == nil {
			current.Config = next
		}
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return installationResult(saved.Config, candidate.PublicURL, ""), nil
}

func pendingInstallation(current Snapshot, pending *PendingSetup) (relayconfig.Config, error) {
	if err := ownsPending(current, pending); err != nil {
		return relayconfig.Config{}, err
	}
	if current.Pending.phase.stage() != (installing{}).stage() {
		return relayconfig.Config{}, ErrSetupSuperseded
	}
	if current.SettingsRevision() != pending.SettingsRevision {
		return relayconfig.Config{}, errSettingsChangedWhilePending
	}
	return current.ReplaceIdentity(pending.profile(pending.credential().AccessKey))
}

func (pending *PendingSetup) profile(accessKey string) relayprofile.Candidate {
	return relayprofile.Candidate{PublicURL: pending.RelayURL, Node: pending.Node, Role: pending.credential().Role, AccessKey: accessKey, ConsoleURL: pending.ConsoleURL, ManagedTurn: pending.ManagedTurn, OfferedFrontend: pending.OfferedFrontend}
}
