package reachability

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestExpiredDemandDoesNotStartFallback(t *testing.T) {
	now := time.Unix(100, 0)
	d := testDemand(now)
	d.Until = now.Add(3 * time.Second)
	g := &fakeGateway{create: func(ctx context.Context, _ Request) (Lease, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second || time.Until(deadline) <= 0 {
			t.Error("probe is not bounded by the remaining live demand")
		}
		if !now.Before(d.Until) {
			t.Error("probe started after demand expiry")
		}
		now = now.Add(2 * time.Second)
		return Lease{}, ErrUnavailable
	}}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g, g, g}})
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	for range 4 {
		a.Reconcile(context.Background())
	}
	if g.creates != 1 {
		t.Fatalf("expired demand started %d probes", g.creates)
	}
	a.Close(context.Background())
}

func TestEquivalentDemandDeadlineAndWithdrawal(t *testing.T) {
	now := time.Unix(100, 0)
	d := testDemand(now)
	d.Until = now.Add(3 * time.Second)
	second := d
	second.ID = "other"
	second.Until = now.Add(5 * time.Second)
	var a *Authority
	g := &fakeGateway{create: func(ctx context.Context, _ Request) (Lease, error) {
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining > 3*time.Second || remaining < 2*time.Second {
			t.Error("equivalent resource did not use its latest live demand")
		}
		a.Withdraw(second.ID)
		if ctx.Err() == nil {
			t.Error("shortened live ownership did not cancel old deadline")
		}
		return Lease{}, ctx.Err()
	}}
	a = New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}, OperationTimeout: 10 * time.Second})
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	if err := a.SetDemand(second); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	g.create = func(ctx context.Context, request Request) (Lease, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Error("remaining equivalent demand did not bound the replacement probe")
		}
		return testLease(request), nil
	}
	a.Reconcile(context.Background())
	if g.creates != 2 || len(a.Facts()) != 1 {
		t.Fatal("shortening a shared demand abandoned the remaining owner")
	}
	a.Close(context.Background())
}

func TestLateLeaseCleanupSurvivesDemandAndParentExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	d := testDemand(now)
	d.Until = now.Add(3 * time.Second)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &fakeGateway{
		create: func(_ context.Context, request Request) (Lease, error) {
			now = now.Add(2 * time.Second)
			cancel()
			return testLease(request), nil
		},
		delete: func(ctx context.Context, _ Request, _ Lease) error {
			deadline, ok := ctx.Deadline()
			if ctx.Err() != nil || !ok || time.Until(deadline) > DefaultOperationTimeout {
				t.Error("late lease cleanup inherited expired demand or unbounded lifetime")
			}
			return nil
		},
	}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}})
	if err := a.SetDemand(d); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(parent)
	now = now.Add(DefaultHeadStart)
	a.Reconcile(parent)
	if g.deletes != 1 || len(a.Facts()) != 0 {
		t.Fatal("late lease leaked")
	}
	a.Close(context.Background())
}

func TestSlowDiscoveryDoesNotBlockRenewalOrRevocation(t *testing.T) {
	now := time.Unix(100, 0)
	renewed, revoked, probing, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	g := &fakeGateway{
		create: func(_ context.Context, request Request) (Lease, error) {
			if request.Endpoint.Local.Port() == 4002 {
				close(probing)
				<-release
				return Lease{}, ErrUnavailable
			}
			return testLease(request), nil
		},
		renew: func(_ context.Context, request Request, _ Lease) (Lease, error) {
			close(renewed)
			return testLease(request), nil
		},
		delete: func(_ context.Context, request Request, _ Lease) error {
			if request.Endpoint.Local.Port() == 4001 {
				close(revoked)
			}
			return nil
		},
	}
	fallback := &fakeGateway{}
	a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g, fallback}, LeaseTTL: 20 * time.Second})
	first := testDemand(now)
	second := first
	second.ID = "revoking"
	second.Endpoint.Local = netip.MustParseAddrPort("192.168.1.5:4001")
	for _, d := range []Demand{first, second} {
		if err := a.SetDemand(d); err != nil {
			t.Fatal(err)
		}
	}
	a.Reconcile(context.Background())
	now = now.Add(DefaultHeadStart)
	a.Reconcile(context.Background())
	third := first
	third.ID = "probing"
	third.Endpoint.Local = netip.MustParseAddrPort("192.168.1.5:4002")
	if err := a.SetDemand(third); err != nil {
		t.Fatal(err)
	}
	a.Withdraw(second.ID)
	a.Reconcile(context.Background())
	now = now.Add(10 * time.Second)
	done := make(chan struct{})
	go func() { a.Reconcile(context.Background()); close(done) }()
	defer func() { close(release); <-done; a.Close(context.Background()) }()
	for _, milestone := range []<-chan struct{}{probing, renewed, revoked} {
		select {
		case <-milestone:
		case <-time.After(time.Second):
			t.Fatal("slow optional discovery blocked an independent maintenance deadline")
		}
	}
	if fallback.creates != 0 {
		t.Fatal("full fallback chain ran in one pass")
	}
}
