package turn

import (
	"context"
	"net/http"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

// Capability is a deployment observation, never a credential or a user policy.
type Capability struct {
	ServiceID string `json:"service_id"`
	Enabled   bool   `json:"enabled"`
}

func (capability Capability) Validate() error {
	if !provider.ValidServiceID(capability.ServiceID) {
		return ErrConfiguration
	}
	return nil
}

// Cache is optional. Runtime authority changes do not depend on durable writes.
type Cache interface {
	Load(senderrelay.Candidate) (*Capability, error)
	Save(context.Context, senderrelay.Candidate, Capability, time.Time) error
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Dependencies struct {
	Client  HTTPClient
	Now     func() time.Time
	Observe func(Event)
	Cache   Cache
}

// Usage is a provider estimate for complete UTC dates, distinct from the local
// transport budget and from an invoice.
type Usage struct {
	FirstDate, LastDate       time.Time
	EgressBytes, IngressBytes uint64
	Sampled                   bool
}
