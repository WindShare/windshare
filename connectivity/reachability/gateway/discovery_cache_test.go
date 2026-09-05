package gateway

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	r "github.com/windshare/windshare/connectivity/reachability"
)

type fakeDHCPSource struct {
	acquire func(context.Context, networkstate.Address) (DHCPOptions, error)
}

func (s fakeDHCPSource) Acquire(ctx context.Context, address networkstate.Address) (DHCPOptions, error) {
	return s.acquire(ctx, address)
}
func discoverySnapshot(observer *networkstate.Observer, resume uint64) networkstate.Snapshot {
	snapshot, _ := observer.Observe(networkstate.State{ResumeSequence: resume, Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.168.1.2"), InterfaceIndex: 7, AdapterID: "adapter"}}, Routes: []networkstate.Route{{InterfaceIndex: 7, Family: 4, Gateway: netip.MustParseAddr("192.168.1.1")}}}, time.Now())
	return snapshot
}
func TestDiscoveryAsyncCacheAndMappedAuthority(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	discovery := NewDiscovery(fakeDHCPSource{acquire: func(context.Context, networkstate.Address) (DHCPOptions, error) {
		once.Do(func() { close(started) })
		<-release
		return DHCPOptions{V4: []byte{4, 192, 168, 1, 9}}, nil
	}})
	defer discovery.Close()
	observer := networkstate.Observer{}
	snapshot := discoverySnapshot(&observer, 0)
	gateways := discovery.Gateways(snapshot)
	client := gateways[0].(*discoveredPCP)
	var used []netip.Addr
	client.fallback.Exchange = func(_ context.Context, _ netip.Addr, server netip.AddrPort, body []byte) ([]byte, error) {
		used = append(used, server.Addr())
		return pcpResponse(body), nil
	}
	lease, err := client.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if len(used) != 1 || used[0].String() != "192.168.1.1" {
		t.Fatal("default route blocked on DHCP")
	}
	close(release)
	select {
	case <-discovery.Changes():
	case <-time.After(time.Second):
		t.Fatal("cache completion not observed")
	}
	lease, err = client.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if used[1].String() != "192.168.1.9" {
		t.Fatal("DHCP precedence", used)
	}
	lease, err = client.Renew(context.Background(), request(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Delete(context.Background(), request(), lease); err != nil {
		t.Fatal(err)
	}
	if used[2].String() != "192.168.1.9" || used[3].String() != "192.168.1.9" {
		t.Fatal("renewal changed server")
	}
	if _, err = client.Renew(context.Background(), request(), r.Lease{}); err == nil {
		t.Fatal("bad token")
	}
	if err = client.Delete(context.Background(), request(), r.Lease{}); err == nil {
		t.Fatal("bad token")
	}
	cached := discovery.lookup(client.key, client.address)
	cached.V4[0] = 0
	if discovery.lookup(client.key, client.address).V4[0] != 4 {
		t.Fatal("mutable cache")
	}
}
func TestDiscoveryRetirementAndWorkerBound(t *testing.T) {
	started := make(chan struct{}, MaxDHCPWorkers)
	release := make(chan struct{})
	discovery := NewDiscovery(fakeDHCPSource{acquire: func(context.Context, networkstate.Address) (DHCPOptions, error) {
		started <- struct{}{}
		<-release
		return DHCPOptions{V4: []byte{4, 192, 168, 1, 9}}, nil
	}})
	observer := networkstate.Observer{}
	first := discovery.Gateways(discoverySnapshot(&observer, 0))[0].(*discoveredPCP)
	_ = discovery.lookup(first.key, first.address)
	<-started
	second := discovery.Gateways(discoverySnapshot(&observer, 1))[0].(*discoveredPCP)
	_ = discovery.lookup(second.key, second.address)
	<-started
	third := discovery.Gateways(discoverySnapshot(&observer, 2))[0].(*discoveredPCP)
	_ = discovery.lookup(third.key, third.address)
	discovery.mu.Lock()
	entries := len(discovery.entries)
	discovery.mu.Unlock()
	if entries != 0 {
		t.Fatal("cancelled blocking calls lost worker capacity")
	}
	discovery.Close()
	close(release)
	// Completion is synchronized through the source capacity rather than sleeping.
	deadline := time.After(time.Second)
	for range MaxDHCPWorkers {
		select {
		case platformDHCPWorkers <- struct{}{}:
		case <-deadline:
			t.Fatal("worker did not finish")
		}
	}
	for range MaxDHCPWorkers {
		<-platformDHCPWorkers
	}
	select {
	case <-discovery.Changes():
		t.Fatal("stale generation published DHCP")
	default:
	}
	if options := discovery.lookup(first.key, first.address); len(options.V4) != 0 {
		t.Fatal("closed discovery")
	}
}
func TestDiscoveryUnavailableDoesNotNotifyAndIPv6Grouping(t *testing.T) {
	done := make(chan struct{})
	discovery := NewDiscovery(fakeDHCPSource{acquire: func(context.Context, networkstate.Address) (DHCPOptions, error) {
		defer close(done)
		return DHCPOptions{}, r.ErrUnavailable
	}})
	observer := networkstate.Observer{}
	client := discovery.Gateways(discoverySnapshot(&observer, 0))[0].(*discoveredPCP)
	_ = discovery.lookup(client.key, client.address)
	<-done
	discovery.Close()
	select {
	case <-discovery.Changes():
		t.Fatal("unavailable created catalog change")
	default:
	}
	if NewDiscovery(nil) == nil {
		t.Fatal("default source")
	}
	copied := cloneDHCP(DHCPOptions{V6: [][]byte{{1, 2}}})
	if len(copied.V6) != 1 {
		t.Fatal("copy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (SystemDHCPSource{}).Acquire(ctx, networkstate.Address{}); err == nil {
		t.Fatal("unsupported/cancelled")
	}
}
