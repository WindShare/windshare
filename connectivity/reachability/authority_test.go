package reachability

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type fakeGateway struct {
	mu                       sync.Mutex
	delete                   func(context.Context, Request, Lease) error
	creates, renews, deletes int
	create                   func(context.Context, Request) (Lease, error)
	renew                    func(context.Context, Request, Lease) (Lease, error)
}

func (g *fakeGateway) Create(ctx context.Context, request Request) (Lease, error) {
	g.mu.Lock()
	g.creates++
	g.mu.Unlock()
	if g.create != nil {
		return g.create(ctx, request)
	}
	return testLease(request), nil
}
func (g *fakeGateway) Renew(ctx context.Context, request Request, lease Lease) (Lease, error) {
	g.mu.Lock()
	g.renews++
	g.mu.Unlock()
	if g.renew != nil {
		return g.renew(ctx, request, lease)
	}
	return testLease(request), nil
}
func (g *fakeGateway) Delete(ctx context.Context, request Request, lease Lease) error {
	g.mu.Lock()
	g.deletes++
	g.mu.Unlock()
	if g.delete != nil {
		return g.delete(ctx, request, lease)
	}
	return nil
}
func testLease(request Request) Lease {
	return Lease{External: netip.MustParseAddrPort("8.8.8.8:55000"), Lifetime: request.Lifetime, GatewayID: "router", ResourceID: "mapping", Kind: "ipv4-mapping"}
}
func testDemand(now time.Time) Demand {
	return Demand{ID: "session/path", Endpoint: Endpoint{Generation: 1, Egress: "7", Local: netip.MustParseAddrPort("192.168.1.5:4000"), Protocol: UDP}, Until: now.Add(time.Minute), Content: true}
}
func TestDemandLeaseLifecycle(t *testing.T) {
	now := time.Unix(1000, 0)
	gateway := &fakeGateway{}
	var events []Event
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{gateway}, LeaseTTL: 20 * time.Second, Observe: func(e Event) { events = append(events, e) }})
	d := testDemand(now)
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	if gateway.creates != 0 {
		t.Fatal("ordinary ICE did not get head start")
	}
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	if gateway.creates != 1 || len(a.Facts()) != 1 || a.Facts()[0].External.Port() != 55000 {
		t.Fatalf("missing actual tuple: %+v", a.Facts())
	}
	facts := a.Facts()
	facts[0].External = netip.AddrPort{}
	if !a.Facts()[0].External.IsValid() {
		t.Fatal("mutable facts")
	}
	now = now.Add(10 * time.Second)
	a.Reconcile(context.Background())
	if gateway.renews != 1 {
		t.Fatal("no renewal")
	}
	d.Direct = true
	d.RetainLease = true
	d.Until = now.Add(time.Minute)
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	a.Reconcile(context.Background())
	if gateway.renews != 2 {
		t.Fatal("admitted lane lost lease")
	}
	a.Withdraw(d.ID)
	a.Reconcile(context.Background())
	if len(a.Facts()) != 0 || gateway.deletes != 0 {
		t.Fatal("grace/publication")
	}
	now = now.Add(DefaultGrace)
	a.Reconcile(context.Background())
	if gateway.deletes != 1 {
		t.Fatal("lease not revoked")
	}
	if len(events) != 4 {
		t.Fatalf("events %d", len(events))
	}
	a.Close(context.Background())
	if !errors.Is(a.SetDemand(testDemand(now)), ErrClosed) {
		t.Fatal("closed authority accepted demand")
	}
}
func TestEquivalentDemandsAndDistinctScopes(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &fakeGateway{}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}})
	first := testDemand(now)
	second := first
	second.ID = "other/session"
	for _, d := range []Demand{first, second} {
		if err := a.SetDemand(d); err != nil {
			t.Fatal(err)
		}
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	if g.creates != 1 {
		t.Fatal("equivalent demands duplicated mapping")
	}
	a.Withdraw(first.ID)
	a.Reconcile(context.Background())
	if g.deletes != 0 || len(a.Facts()) != 1 {
		t.Fatal("other path lost retained mapping")
	}
	third := second
	third.ID = "restricted"
	third.Scope.Remote = netip.MustParseAddrPort("1.1.1.1:4000")
	if err := a.SetDemand(third); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	if g.creates != 2 || len(a.Facts()) != 2 {
		t.Fatal("restricted scope incorrectly merged")
	}
	a.Retire(1)
	a.Reconcile(context.Background())
	if g.deletes != 2 || len(a.Facts()) != 0 {
		t.Fatal("retired generation retained facts")
	}
	if err := a.SetDemand(second); !errors.Is(err, ErrInvalid) {
		t.Fatal("retired generation accepted demand")
	}
}
func TestInvalidLeaseFallbackAndDemandLimits(t *testing.T) {
	now := time.Unix(1000, 0)
	bad := &fakeGateway{create: func(_ context.Context, r Request) (Lease, error) {
		lease := testLease(r)
		lease.External = netip.MustParseAddrPort("100.64.0.1:5000")
		return lease, nil
	}}
	good := &fakeGateway{}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{bad, good}, Capacity: 1})
	d := testDemand(now)
	d.Until = now.Add(time.Hour)
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	other := d
	other.ID = "other"
	if err := a.SetDemand(other); !errors.Is(err, ErrCapacity) {
		t.Fatal("capacity")
	}
	badDemand := d
	badDemand.Content = false
	if err := a.SetDemand(badDemand); !errors.Is(err, ErrInvalid) {
		t.Fatal("browse mapping")
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	if bad.deletes != 1 || good.creates != 0 {
		t.Fatal("fallback monopolized a maintenance pass")
	}
	a.Reconcile(context.Background())
	if good.creates != 1 {
		t.Fatal("bad/private result not cleaned before fallback")
	}
	now = now.Add(time.Minute)
	a.Reconcile(context.Background())
	now = now.Add(DefaultGrace)
	a.Reconcile(context.Background())
	if good.deletes != 1 {
		t.Fatal("unbounded demand TTL")
	}
}
func TestLateCompletionAfterWithdrawRevoked(t *testing.T) {
	now := time.Unix(1000, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	g := &fakeGateway{create: func(ctx context.Context, r Request) (Lease, error) {
		close(started)
		<-release
		return testLease(r), nil
	}}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}})
	if err := a.SetDemand(testDemand(now)); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	done := make(chan struct{})
	go func() { defer close(done); a.Reconcile(context.Background()) }()
	<-started
	a.Withdraw("session/path")
	close(release)
	<-done
	if g.deletes != 1 || len(a.Facts()) != 0 {
		t.Fatal("late mapping leaked")
	}
}
func TestRenewFailureExpiryAndIPv6(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &fakeGateway{renew: func(context.Context, Request, Lease) (Lease, error) { return Lease{}, ErrUnavailable }}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}, LeaseTTL: 10 * time.Second})
	d := testDemand(now)
	d.Endpoint.Local = netip.MustParseAddrPort("[2606:4700::1]:4000")
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	if len(a.Facts()) != 1 {
		t.Fatal("native IPv6 missing")
	}
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	now = now.Add(10 * time.Second)
	a.Reconcile(context.Background())
	if len(a.Facts()) != 1 {
		t.Fatal("expired mapping retained")
	}
	a.Close(context.Background())
}
func TestPublicAddress(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1", "2001:4860::1"} {
		if !PublicAddress(netip.MustParseAddr(s)) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"0.0.0.0", "127.0.0.1", "192.168.1.1", "100.64.1.2", "192.0.2.1", "198.51.100.1", "203.0.113.1", "240.1.2.3", "fe80::1", "fc00::1", "2001:db8::1", "2002::1", "3fff::1", "::1"} {
		if PublicAddress(netip.MustParseAddr(s)) {
			t.Fatal(s)
		}
	}
}
func TestConcurrentReconcileCreatesOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &fakeGateway{}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}})
	if err := a.SetDemand(testDemand(now)); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { a.Reconcile(context.Background()) })
	}
	wg.Wait()
	if g.creates != 1 {
		t.Fatal("parallel duplicate mapping")
	}
	a.Close(context.Background())
}
