package reachability

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestLiveDemandTimerCancelsGatewayWork(t *testing.T) {
	for _, operation := range []string{"create", "renew-retained-direct"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var canceled bool
				awaitExpiry := func(ctx context.Context) (Lease, error) {
					<-ctx.Done()
					canceled = errors.Is(ctx.Err(), context.DeadlineExceeded)
					return Lease{}, ctx.Err()
				}
				g := &fakeGateway{
					create: func(ctx context.Context, request Request) (Lease, error) {
						if operation == "create" {
							return awaitExpiry(ctx)
						}
						return testLease(request), nil
					},
					renew: func(ctx context.Context, _ Request, _ Lease) (Lease, error) { return awaitExpiry(ctx) },
					delete: func(ctx context.Context, _ Request, _ Lease) error {
						if ctx.Err() != nil {
							t.Error("expiry canceled independent revocation")
						}
						return nil
					},
				}
				a := New(Config{Gateways: []Gateway{g, g}, LeaseTTL: 2 * time.Second, OperationTimeout: 10 * time.Second})
				d := testDemand(time.Now())
				d.Until = time.Now().Add(3 * time.Second)
				if err := a.SetDemand(d); err != nil {
					t.Fatal(err)
				}
				a.Reconcile(context.Background())
				time.Sleep(DefaultHeadStart)
				if operation != "create" {
					a.Reconcile(context.Background())
					d.Direct = true
					d.RetainLease = true
					d.Until = time.Now().Add(1500 * time.Millisecond)
					if err := a.SetDemand(d); err != nil {
						t.Fatal(err)
					}
					time.Sleep(time.Second)
				}
				a.Reconcile(context.Background())
				if !canceled || time.Now() != d.Until {
					t.Fatal("work did not stop at live demand deadline")
				}
				a.Reconcile(context.Background())
				time.Sleep(DefaultGrace)
				a.Reconcile(context.Background())
				if len(a.Facts()) != 0 || g.creates != 1 {
					t.Fatal("expired demand authorized publication/fallback")
				}
				if operation != "create" && g.deletes != 1 {
					t.Fatal("retained direct lease escaped demand expiry")
				}
				a.Close(context.Background())
			})
		})
	}
}
