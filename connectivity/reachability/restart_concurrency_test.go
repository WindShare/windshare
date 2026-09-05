package reachability

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestRestartCannotBeOverwrittenByInflightOldEpoch(t *testing.T) {
	for _, mode := range []string{"creation", "renewal"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(100, 0)
			oldStarted, restarted := make(chan struct{}), make(chan struct{})
			maintenance, fresh := false, false
			leaseAt := func(request Request, epoch uint32) Lease {
				lease := testLease(request)
				lease.ServerEpoch = epoch
				return lease
			}
			wait := func(milestone <-chan struct{}) {
				select {
				case <-milestone:
				case <-time.After(time.Second):
					t.Error("missing restart ordering milestone")
				}
			}
			oldResponse := func(request Request) (Lease, error) {
				close(oldStarted)
				wait(restarted)
				return leaseAt(request, 100), nil
			}
			g := &fakeGateway{
				create: func(_ context.Context, request Request) (Lease, error) {
					if fresh {
						return leaseAt(request, 1), nil
					}
					if maintenance && request.Endpoint.Local.Port() == 4001 {
						return oldResponse(request)
					}
					return leaseAt(request, 100), nil
				},
				renew: func(_ context.Context, request Request, _ Lease) (Lease, error) {
					if fresh {
						return leaseAt(request, 1), nil
					}
					if request.Endpoint.Local.Port() == 4001 {
						return oldResponse(request)
					}
					wait(oldStarted)
					lease := leaseAt(request, 1)
					lease.ServerRestarted = true
					return lease, nil
				},
			}
			a := New(Config{Now: func() time.Time { return now }, Gateways: []Gateway{g}, LeaseTTL: 20 * time.Second, Observe: func(event Event) {
				if event.ServerRestarted {
					close(restarted)
				}
			}})
			defer a.Close(context.Background())
			first := testDemand(now)
			second := first
			second.ID = "second"
			second.Endpoint.Local = netip.MustParseAddrPort("192.168.1.5:4001")
			if err := a.SetDemand(first); err != nil {
				t.Fatal(err)
			}
			if mode == "renewal" {
				if err := a.SetDemand(second); err != nil {
					t.Fatal(err)
				}
			}
			a.Reconcile(context.Background())
			now = now.Add(DefaultHeadStart)
			a.Reconcile(context.Background())
			if mode == "creation" {
				if err := a.SetDemand(second); err != nil {
					t.Fatal(err)
				}
				a.Reconcile(context.Background())
			}
			now = now.Add(10 * time.Second)
			maintenance = true
			a.Reconcile(context.Background())
			if facts := a.Facts(); len(facts) != 1 || facts[0].Endpoint != first.Endpoint {
				t.Fatal("late pre-restart reply resurrected mapping", facts)
			}
			fresh = true
			a.Reconcile(context.Background())
			if len(a.Facts()) != 2 {
				t.Fatal("superseded resource was not reacquired")
			}
		})
	}
}
