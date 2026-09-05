// Package reachability owns demand-bounded gateway leases, never ICE attempts.
package reachability

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

type Protocol uint8

const (
	TCP Protocol = 6
	UDP Protocol = 17
)
const (
	DefaultCapacity         = 64
	DefaultDemandTTL        = 60 * time.Second
	DefaultLeaseTTL         = 120 * time.Second
	DefaultHeadStart        = 2 * time.Second
	DefaultGrace            = 3 * time.Second
	DefaultOperationTimeout = 2 * time.Second
)

var (
	ErrInvalid         = errors.New("invalid reachability request")
	ErrCapacity        = errors.New("reachability capacity exhausted")
	ErrUnavailable     = errors.New("gateway capability unavailable")
	ErrInvalidResponse = errors.New("invalid gateway response")
	ErrClosed          = errors.New("reachability authority closed")
	// ErrLeaseLost means a renewal no longer authorizes the previous endpoint.
	// Other errors retain the previous lease only until its original expiry.
	ErrLeaseLost = errors.New("gateway lease lost")
)

type Endpoint struct {
	Generation uint64
	Egress     string
	Local      netip.AddrPort
	Protocol   Protocol
}

// A zero Remote permits every peer. Restricted and wildcard permissions cannot
// be merged even when they expose the same external endpoint.
type Scope struct{ Remote netip.AddrPort }
type Request struct {
	Endpoint Endpoint
	Scope    Scope
	Lifetime time.Duration
}
type Lease struct {
	External        netip.AddrPort
	Lifetime        time.Duration
	GatewayID       string
	ResourceID      string
	Kind            string
	Token           any
	ServerEpoch     uint32
	ServerRestarted bool
}

// Gateway implementations must respect context cancellation and support calls
// for independent resources concurrently. Renewal errors wrap ErrLeaseLost only
// when the previous mapping has been revoked or is known to be absent.
type Gateway interface {
	Create(context.Context, Request) (Lease, error)
	Renew(context.Context, Request, Lease) (Lease, error)
	Delete(context.Context, Request, Lease) error
}
type Demand struct {
	ID          string
	Endpoint    Endpoint
	Scope       Scope
	Until       time.Time
	Content     bool
	Direct      bool
	RetainLease bool
}
type Fact struct {
	Endpoint  Endpoint
	Scope     Scope
	External  netip.AddrPort
	ExpiresAt time.Time
	Kind      string
	GatewayID string
}
type Event struct {
	Kind            string
	Endpoint        Endpoint
	Scope           Scope
	GatewayID       string
	Error           error
	ServerEpoch     uint32
	ServerRestarted bool
}
type Config struct {
	Now              func() time.Time
	Gateways         []Gateway
	Capacity         int
	DemandTTL        time.Duration
	LeaseTTL         time.Duration
	HeadStart        time.Duration
	Grace            time.Duration
	OperationTimeout time.Duration
	Observe          func(Event)
}

func validEndpoint(e Endpoint) bool {
	return e.Generation > 0 && e.Egress != "" && e.Local.IsValid() && e.Local.Port() != 0 && !e.Local.Addr().IsUnspecified() && !e.Local.Addr().IsMulticast() && (e.Protocol == UDP || e.Protocol == TCP)
}

var excludedPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

// PublicAddress is deliberately stricter than netip.IsGlobalUnicast, which
// includes private and documentation addresses that cannot establish reachability.
func PublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, prefix := range excludedPublic {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
