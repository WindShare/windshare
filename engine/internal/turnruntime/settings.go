// Package turnruntime owns TURN settings, shared account budgets and demand-driven discovery.
package turnruntime

import (
	"errors"
	"slices"

	"github.com/windshare/windshare/engine/internal/turnruntime/internal/provider"
	"github.com/windshare/windshare/engine/relayprofile"
)

// Settings is one desired configuration. Discovered capabilities are observations,
// not user settings, so a refresh cannot overwrite a budget or restore authority.
type Settings struct {
	Manual  ManualSettings
	Managed ManagedSettings
}

// ManagedSettings projects deployment authority from the application's relay
// profiles. TURN does not own relay selection, frontend settings or enrollment.
type ManagedSettings struct {
	Relays   []relayprofile.Candidate
	Policies []ManagedPolicy
}

type ManagedPolicy struct {
	RelayURL         string `json:"relay_url"`
	LocalBudgetBytes uint64 `json:"local_budget_bytes"`
}

func (policy ManagedPolicy) Validate() error {
	_, err := provider.ManagedEndpoint(policy.RelayURL)
	if err != nil {
		return errors.Join(ErrConfiguration, err)
	}
	return nil
}

func (settings Settings) Clone() Settings {
	settings.Manual = settings.Manual.Clone()
	settings.Managed.Relays = slices.Clone(settings.Managed.Relays)
	settings.Managed.Policies = slices.Clone(settings.Managed.Policies)
	return settings
}

func (settings Settings) Validate() error {
	_, err := prepareSettings(settings)
	return err
}

func (settings ManagedSettings) Validate() error {
	if len(settings.Relays) > MaximumManagedServices || len(settings.Policies) > MaximumManagedServices {
		return ErrConfiguration
	}
	if _, err := relayprofile.PublicURLs(settings.Relays); err != nil {
		return errors.Join(ErrConfiguration, err)
	}
	relays := make(map[string]bool, len(settings.Relays))
	for _, relay := range settings.Relays {
		relays[addressKey(relay.PublicURL)] = true
	}
	policies := make(map[string]bool, len(settings.Policies))
	for _, policy := range settings.Policies {
		address := addressKey(policy.RelayURL)
		if policy.Validate() != nil || !relays[address] || policies[address] {
			return ErrConfiguration
		}
		policies[address] = true
	}
	return nil
}

type preparedSettings struct {
	settings Settings
	manual   manualPlan
}

func prepareSettings(settings Settings) (preparedSettings, error) {
	settings = settings.Clone()
	manual, err := settings.Manual.prepare()
	if err != nil {
		return preparedSettings{}, err
	}
	if err := settings.Managed.Validate(); err != nil {
		return preparedSettings{}, err
	}
	return preparedSettings{settings: settings, manual: manual}, nil
}
