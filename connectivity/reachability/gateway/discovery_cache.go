package gateway

import (
	"context"
	"net/netip"
	"slices"
	"sync"

	"github.com/windshare/windshare/connectivity/networkstate"
	r "github.com/windshare/windshare/connectivity/reachability"
)

const MaxDHCPWorkers = 2
const MaxDiscoveryEntries = 16

var platformDHCPWorkers = make(chan struct{}, MaxDHCPWorkers)

type DHCPOptions struct {
	V4 []byte
	V6 [][]byte
}
type DHCPSource interface {
	Acquire(context.Context, networkstate.Address) (DHCPOptions, error)
}
type SystemDHCPSource struct{}
type discoveryKey struct {
	generation uint64
	index      int
}
type discoveryEntry struct {
	options DHCPOptions
	ready   bool
	cancel  context.CancelFunc
}

// Discovery belongs to the process, not an attempt. A blocking OS DHCP call
// retains its capacity slot until the call really returns, even after retirement.
type Discovery struct {
	mu         sync.Mutex
	source     DHCPSource
	entries    map[discoveryKey]*discoveryEntry
	generation uint64
	closed     bool
	changes    chan struct{}
}

func NewDiscovery(source DHCPSource) *Discovery {
	if source == nil {
		source = SystemDHCPSource{}
	}
	return &Discovery{source: source, entries: make(map[discoveryKey]*discoveryEntry), changes: make(chan struct{}, 1)}
}
func (d *Discovery) Changes() <-chan struct{} { return d.changes }
func (d *Discovery) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for _, entry := range d.entries {
		entry.cancel()
	}
	clear(d.entries)
}
func (d *Discovery) Gateways(snapshot networkstate.Snapshot) []r.Gateway {
	d.mu.Lock()
	if snapshot.GenerationID() > d.generation {
		d.generation = snapshot.GenerationID()
		for key, entry := range d.entries {
			if key.generation < d.generation {
				entry.cancel()
				delete(d.entries, key)
			}
		}
	}
	d.mu.Unlock()
	gateways := Defaults(snapshot)
	for i, client := range gateways {
		pcp, ok := client.(*PCP)
		if !ok {
			continue
		}
		var address networkstate.Address
		for _, candidate := range snapshot.Addresses() {
			if EgressID(candidate.InterfaceIndex) == pcp.Egress && candidate.IP.Is4() == pcp.Server.Addr().Is4() {
				address = candidate
				break
			}
		}
		if address.IP.IsValid() {
			for _, candidate := range snapshot.Addresses() {
				if candidate.InterfaceIndex == address.InterfaceIndex && candidate.IP.Is4() {
					address = candidate
					break
				}
			}
			gateways[i] = &discoveredPCP{discovery: d, key: discoveryKey{snapshot.GenerationID(), address.InterfaceIndex}, address: address, fallback: pcp}
		}
	}
	return gateways
}
func (d *Discovery) lookup(key discoveryKey, address networkstate.Address) DHCPOptions {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || key.generation != d.generation {
		return DHCPOptions{}
	}
	if entry := d.entries[key]; entry != nil {
		if entry.ready {
			return cloneDHCP(entry.options)
		}
		return DHCPOptions{}
	}
	if len(d.entries) >= MaxDiscoveryEntries {
		return DHCPOptions{}
	}
	select {
	case platformDHCPWorkers <- struct{}{}:
	default:
		return DHCPOptions{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &discoveryEntry{cancel: cancel}
	d.entries[key] = entry
	go func() {
		defer func() { <-platformDHCPWorkers }()
		options, err := d.source.Acquire(ctx, address)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed || d.entries[key] != entry || key.generation != d.generation {
			return
		}
		entry.ready = true
		if err == nil {
			// Admission validates grouping and capacities before data becomes eligible.
			_, err = DiscoverPCP(PCPDiscovery{Egress: EgressID(address.InterfaceIndex), DHCPv4: options.V4, DHCPv6: options.V6})
			if err == nil {
				entry.options = cloneDHCP(options)
			}
		}
		if len(entry.options.V4) > 0 || len(entry.options.V6) > 0 {
			select {
			case d.changes <- struct{}{}:
			default:
			}
		}
	}()
	return DHCPOptions{}
}
func cloneDHCP(options DHCPOptions) DHCPOptions {
	result := DHCPOptions{V4: slices.Clone(options.V4)}
	for _, data := range options.V6 {
		result.V6 = append(result.V6, slices.Clone(data))
	}
	return result
}

type discoveredPCP struct {
	discovery *Discovery
	key       discoveryKey
	address   networkstate.Address
	fallback  *PCP
}
type discoveredToken struct {
	server netip.AddrPort
	token  any
}

func (p *discoveredPCP) Create(ctx context.Context, request r.Request) (r.Lease, error) {
	if err := ctx.Err(); err != nil {
		return r.Lease{}, err
	}
	if request.Endpoint.Egress != p.fallback.Egress || request.Endpoint.Local.Addr().Is4() != p.fallback.Server.Addr().Is4() {
		return r.Lease{}, r.ErrUnavailable
	}
	options := p.discovery.lookup(p.key, p.address)
	if request.Endpoint.Local.Addr().Is6() {
		options.V4 = nil
	}
	servers, err := DiscoverPCP(PCPDiscovery{Egress: p.fallback.Egress, DHCPv4: options.V4, DHCPv6: options.V6, DefaultRouters: []netip.Addr{p.fallback.Server.Addr()}})
	if err != nil {
		return r.Lease{}, err
	}
	for _, server := range servers {
		for _, address := range server.Addresses {
			if address.Is4() != request.Endpoint.Local.Addr().Is4() {
				continue
			}
			client := *p.fallback
			client.Server = netip.AddrPortFrom(address, controlPort)
			lease, err := client.Create(ctx, request)
			if err == nil {
				lease.Token = discoveredToken{server: client.Server, token: lease.Token}
				return lease, nil
			}
			if ctx.Err() != nil {
				return r.Lease{}, ctx.Err()
			}
		}
	}
	return r.Lease{}, r.ErrUnavailable
}
func (p *discoveredPCP) Renew(ctx context.Context, request r.Request, lease r.Lease) (r.Lease, error) {
	token, ok := lease.Token.(discoveredToken)
	if !ok {
		return r.Lease{}, r.ErrInvalid
	}
	client := *p.fallback
	client.Server = token.server
	lease.Token = token.token
	lease, err := client.Renew(ctx, request, lease)
	if err == nil {
		lease.Token = discoveredToken{server: client.Server, token: lease.Token}
	}
	return lease, err
}
func (p *discoveredPCP) Delete(ctx context.Context, request r.Request, lease r.Lease) error {
	token, ok := lease.Token.(discoveredToken)
	if !ok {
		return r.ErrInvalid
	}
	client := *p.fallback
	client.Server = token.server
	lease.Token = token.token
	return client.Delete(ctx, request, lease)
}
