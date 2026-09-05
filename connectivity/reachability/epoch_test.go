package reachability

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestRestartInvalidatesOlderGatewayFacts(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &fakeGateway{create: func(_ context.Context, request Request) (Lease, error) {
		lease := testLease(request)
		lease.ServerEpoch = 100
		return lease, nil
	}, renew: func(_ context.Context, request Request, old Lease) (Lease, error) {
		lease := testLease(request)
		lease.ServerEpoch = 1
		lease.ServerRestarted = old.ServerEpoch > 1
		return lease, nil
	}}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}, LeaseTTL: 20 * time.Second})
	first := testDemand(now)
	second := first
	second.ID = "second"
	second.Endpoint.Local = netip.MustParseAddrPort("192.168.1.5:4001")
	for _, d := range []Demand{first, second} {
		if err := a.SetDemand(d); err != nil {
			t.Fatal(err)
		}
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	now = now.Add(10 * time.Second)
	a.Reconcile(context.Background())
	if len(a.Facts()) != 2 || g.renews != 2 {
		t.Fatal("gateway restart did not reacquire all old resources")
	}
	a.Reconcile(context.Background())
	if g.renews != 2 {
		t.Fatal("fresh epoch invalidated by sibling renewal")
	}
	a.Close(context.Background())
}
func TestDiscoveryCompletingDuringProbeIsNotLost(t *testing.T) {
	now := time.Unix(1000, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	g := &fakeGateway{create: func(_ context.Context, request Request) (Lease, error) {
		close(started)
		<-release
		return Lease{}, ErrUnavailable
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
	a.RefreshUnavailable()
	close(release)
	<-done
	g.create = nil
	a.Reconcile(context.Background())
	if g.creates != 2 || len(a.Facts()) != 1 {
		t.Fatal("inflight discovery wake lost")
	}
	a.Close(context.Background())
}
