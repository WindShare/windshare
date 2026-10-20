package turnconfig

import "github.com/windshare/windshare/engine/turnprovider"

// ManagedPolicy is the user's budget for a deployment. Discovery never writes it.
type ManagedPolicy struct {
	RelayURL         string `json:"relay_url"`
	LocalBudgetBytes uint64 `json:"local_budget_bytes"`
}

func (policy ManagedPolicy) Validate() error {
	_, err := turnprovider.ManagedEndpoint(policy.RelayURL)
	return err
}
