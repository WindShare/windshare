package reachability

import (
	"context"
	"testing"
	"time"
)

func TestDiscoveryRefreshIsOnlyForUnavailableDemand(t *testing.T) {
	now := time.Unix(1000, 0)
	available := false
	g := &fakeGateway{create: func(_ context.Context, request Request) (Lease, error) {
		if !available {
			return Lease{}, ErrUnavailable
		}
		return testLease(request), nil
	}}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}})
	if err := a.SetDemand(testDemand(now)); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	a.Reconcile(context.Background())
	if g.creates != 1 {
		t.Fatal("unbounded gateway retry")
	}
	available = true
	a.RefreshUnavailable()
	a.Reconcile(context.Background())
	if g.creates != 2 || len(a.Facts()) != 1 {
		t.Fatal("catalog change did not retry unavailable resource")
	}
	a.RefreshUnavailable()
	a.Reconcile(context.Background())
	if g.creates != 2 {
		t.Fatal("catalog change replaced held lease")
	}
	a.Close(context.Background())
}
