package turnconfig

import "github.com/windshare/windshare/engine/turnprovider"

// ManagedService is deployment-owned inventory, separate from device-owned
// issuer secrets. Access authority comes from the matching saved relay profile.
type ManagedService struct {
	ServiceID        string `json:"service_id"`
	RelayURL         string `json:"relay_url"`
	Enabled          bool   `json:"enabled"`
	LocalBudgetBytes uint64 `json:"local_budget_bytes"`
}

func (service ManagedService) Validate() error {
	if !turnprovider.ValidServiceID(service.ServiceID) || len(service.ServiceID) > 48 {
		return ErrConfiguration
	}
	_, err := turnprovider.ManagedEndpoint(service.RelayURL)
	return err
}
