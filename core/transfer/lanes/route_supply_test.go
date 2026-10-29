package lanes

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

func retiredBlockFailure() error {
	value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionTransport)
	return NewDemandReassignableAfterRetirement(fault.Wrap(value, errors.New("fragment inactivity")))
}

func TestForbiddenRelayCannotPromiseRecovery(t *testing.T) {
	for _, hasDirect := range []bool{false, true} {
		name := "no permitted lane"
		if hasDirect {
			name = "direct generation exhausted"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lanes, err := New(Config{ContentRoutePolicy: ContentRouteDirectOnly,
					ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](61)})
				if err != nil {
					t.Fatal(err)
				}
				defer lanes.Close()
				if _, err := lanes.ExpectRoute(RouteRelay); !errors.Is(err, ErrNoContentRoute) {
					t.Fatalf("forbidden route acquired recovery ownership: %v", err)
				}
				if err := lanes.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
					t.Error("p2p-only issued content to relay")
					return records.BlockRecord{}, nil
				})); err != nil {
					t.Fatal(err)
				}
				failure := retiredBlockFailure()
				if hasDirect {
					if err := lanes.Add(Identity{ID: 2, Epoch: 1}, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
						return records.BlockRecord{}, failure
					})); err != nil {
						t.Fatal(err)
					}
				}
				demand := validDemand(t, transferDescriptor(t, 1), 0)
				done := make(chan error, 1)
				go func() {
					_, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
					done <- err
				}()
				synctest.Wait()
				select {
				case err := <-done:
					want := ErrNoContentRoute
					if hasDirect {
						want = failure
					}
					if !errors.Is(err, want) {
						t.Fatalf("route exhaustion lost failure: %v", err)
					}
				default:
					t.Fatal("permanently forbidden relay left exhausted content waiting")
				}
			})
		})
	}
}

func TestRouteSupplyOwnsReplacementWait(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "owner ends"
		if replace {
			name = "replacement admitted"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lanes, err := New(Config{ContentRoutePolicy: ContentRouteDirectOnly,
					ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](62)})
				if err != nil {
					t.Fatal(err)
				}
				defer lanes.Close()
				supply, err := lanes.ExpectRoute(RouteDirect)
				if err != nil {
					t.Fatal(err)
				}
				defer supply.Close()
				initial := Identity{ID: 2, Epoch: 1}
				failure := retiredBlockFailure()
				if err := lanes.Add(initial, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
					return records.BlockRecord{}, failure
				})); err != nil {
					t.Fatal(err)
				}
				descriptor := transferDescriptor(t, 1)
				demand := validDemand(t, descriptor, 0)
				done := make(chan error, 1)
				go func() {
					block, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
					if err == nil && block.Descriptor() != descriptor {
						t.Error("replacement did not deliver the original immutable demand")
					}
					done <- err
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("fetch ended while its recovery owner was live: %v", err)
				default:
				}
				if got := lanes.RecoveryDemand(); !slices.Equal(got, []Identity{initial}) {
					t.Fatalf("recovery demand=%v", got)
				}
				if replace {
					record := transferRecord(t, descriptor, 0)
					if err := lanes.Add(Identity{ID: initial.ID, Epoch: initial.Epoch + 1}, RouteDirect,
						laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return record, nil })); err != nil {
						t.Fatal(err)
					}
				} else {
					supply.Close()
					supply.Close()
				}
				synctest.Wait()
				select {
				case err := <-done:
					if replace && err != nil || !replace && !errors.Is(err, failure) {
						t.Fatalf("replacement=%t result=%v", replace, err)
					}
				default:
					t.Fatal("route ownership transition did not wake queued demand")
				}
				if got := lanes.RecoveryDemand(); len(got) != 0 {
					t.Fatalf("settled demand retained recovery authority: %v", got)
				}
			})
		})
	}
}

func TestRouteSupplyOwnsInitialWait(t *testing.T) {
	for _, transition := range []string{"owner ends", "caller cancels", "set stops", "lane arrives"} {
		t.Run(transition, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lanes := newBoundaryLaneSet(t, 63)
				// An empty set may wait only while a connectivity owner promises a
				// permitted route; without this capability it is already exhausted.
				supply, err := lanes.ExpectRoute(RouteDirect)
				if err != nil {
					t.Fatal(err)
				}
				defer supply.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				descriptor := transferDescriptor(t, 1)
				demand := validDemand(t, descriptor, 0)
				done := make(chan error, 1)
				go func() {
					_, err := lanes.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand)})
					done <- err
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("fetch ended before route transition: %v", err)
				default:
				}

				var wantErr error
				switch transition {
				case "owner ends":
					supply.Close()
					wantErr = ErrNoContentRoute
				case "caller cancels":
					cancel()
					wantErr = context.Canceled
				case "set stops":
					lanes.Stop()
					wantErr = ErrClosed
				case "lane arrives":
					record := transferRecord(t, descriptor, 0)
					if err := lanes.Add(Identity{ID: 1, Epoch: 1}, RouteDirect,
						laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return record, nil })); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				select {
				case err := <-done:
					if !errors.Is(err, wantErr) {
						t.Fatalf("route transition result = %v, want %v", err, wantErr)
					}
				default:
					t.Fatal("route transition did not resolve queued demand")
				}
			})
		})
	}
}

func TestConsumerBackpressureDoesNotRequestPathRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lanes, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](64)})
		if err != nil {
			t.Fatal(err)
		}
		defer lanes.Close()
		supply, err := lanes.ExpectRoute(RouteDirect)
		if err != nil {
			t.Fatal(err)
		}
		defer supply.Close()
		var blocked atomic.Bool
		changed := make(chan struct{})
		identity := Identity{ID: 2, Epoch: 1}
		if err := lanes.Add(identity, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			blocked.Store(true)
			return records.BlockRecord{}, retiredBlockFailure()
		})); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		demand := validDemand(t, transferDescriptor(t, 1), 0)
		done := make(chan error, 1)
		go func() {
			_, err := lanes.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand), Ready: func() CapacityReadiness {
				if blocked.Load() {
					return CapacityReadiness{Kind: CapacityConsumer, Changed: changed}
				}
				return CapacityReadiness{Kind: CapacityReady}
			}})
			done <- err
		}()
		synctest.Wait()
		if got := lanes.RecoveryDemand(); len(got) != 0 {
			t.Fatalf("backpressure triggered recovery: %v", got)
		}
		blocked.Store(false)
		close(changed)
		synctest.Wait()
		if got := lanes.RecoveryDemand(); !slices.Equal(got, []Identity{identity}) {
			t.Fatalf("ready demand=%v", got)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel recovery=%v", err)
		}
	})
}
