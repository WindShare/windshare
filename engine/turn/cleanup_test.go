package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

type cleanupSource func(context.Context, provider.Subject) (provider.Acquisition, error)

func (source cleanupSource) Issue(ctx context.Context, subject provider.Subject) (provider.Acquisition, error) {
	return source(ctx, subject)
}

type managedLeaseRequest struct {
	ServiceID       string `json:"service_id"`
	LeaseID         string `json:"lease_id"`
	SessionID       string `json:"session_id"`
	PathID          string `json:"path_id"`
	PreviousLeaseID string `json:"previous_lease_id"`
}

type lostGrantClient struct {
	mu              sync.Mutex
	grants          map[string][]byte
	loseGrants      bool
	lostRevocations int
	revocations     []managedLeaseRequest
}

func (client *lostGrantClient) Do(request *http.Request) (*http.Response, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	var body managedLeaseRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		return nil, err
	}
	if request.URL.Path == "/api/turn/revoke" {
		client.revocations = append(client.revocations, body)
		_, existed := client.grants[body.LeaseID]
		delete(client.grants, body.LeaseID)
		if client.lostRevocations > 0 {
			client.lostRevocations--
			return nil, io.ErrUnexpectedEOF
		}
		if existed {
			return reply(http.StatusOK, `{"state":"revoked"}`), nil
		}
		return reply(http.StatusOK, `{"state":"canceled"}`), nil
	}
	grant, exists := client.grants[body.LeaseID]
	if !exists {
		var err error
		grant, err = json.Marshal(map[string]any{
			"service_id": body.ServiceID, "lease_id": body.LeaseID,
			"session_id": body.SessionID, "path_id": body.PathID,
			"urls": []string{"turn:localhost:3478"}, "username": body.LeaseID,
			"password": "password", "expires_at": time.Now().Add(time.Hour).UnixMilli(),
		})
		if err != nil {
			return nil, err
		}
		client.grants[body.LeaseID] = grant
	}
	if client.loseGrants {
		return nil, io.ErrUnexpectedEOF
	}
	return reply(http.StatusCreated, string(grant)), nil
}

func TestLostManagedIssuanceRetainsOwnershipThroughLostCleanupReply(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		t.Run(map[bool]string{false: "issue", true: "renew"}[renewal], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &lostGrantClient{grants: make(map[string][]byte)}
				source, err := provider.NewRemote(provider.RemoteConfig{
					RelayURL: "https://relay.example", AccessKey: "device-key", ServiceID: "cf-service", Client: client,
				})
				if err != nil {
					t.Fatal(err)
				}
				manager := newConfiguredManager(t, configurationFor(source, 0))
				subject := turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}
				var previous *turnpath.Lease
				expectedActive := 0
				if renewal {
					previous, err = manager.Acquire(t.Context(), "primary", subject)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(previous.Close)
					expectedActive = 1
					subject.PathID = [16]byte{3}
				}
				client.loseGrants, client.lostRevocations = true, 1
				var acquired *turnpath.Lease
				if renewal {
					acquired, err = previous.Renew(t.Context(), subject)
				} else {
					acquired, err = manager.Acquire(t.Context(), "primary", subject)
				}
				if acquired != nil {
					acquired.Close()
					t.Fatal("unknown grant became usable")
				}
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
				synctest.Wait()
				if state := manager.Snapshot()[0]; state.ActiveLeases != expectedActive || state.PendingCleanup != 1 {
					t.Fatalf("lost reply discarded ownership: %+v", state)
				}
				time.Sleep(cleanupRetryDelay)
				synctest.Wait()
				if state := manager.Snapshot()[0]; state.PendingCleanup != 0 || state.ActiveLeases != expectedActive {
					t.Fatalf("cleanup did not converge: %+v", state)
				}
				client.mu.Lock()
				if len(client.grants) != expectedActive || len(client.revocations) != 2 ||
					client.revocations[0] != client.revocations[1] || client.revocations[0].PreviousLeaseID != "" {
					t.Errorf("cleanup changed identity or targeted the previous lease: %+v", client.revocations)
				}
				client.mu.Unlock()
				if previous != nil {
					if previous.Loss() != nil {
						t.Fatal("failed renewal revoked the original path")
					}
					previous.Close()
				}
				if err := manager.Close(); err != nil {
					t.Fatal(err)
				}
				client.mu.Lock()
				defer client.mu.Unlock()
				if len(client.grants) != 0 {
					t.Fatal("remote leases survived completed cleanup")
				}
			})
		})
	}
}

func TestUnresolvedCleanupBoundsCapacityAndSurvivesReconfiguration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var available atomic.Bool
		var calls atomic.Int32
		source := cleanupSource(func(_ context.Context, subject provider.Subject) (provider.Acquisition, error) {
			resource := &provider.Cleanup{ID: fmt.Sprintf("%x", subject.PathID), ExpiresAt: time.Now().Add(time.Hour),
				Revoke: func(context.Context) (provider.CleanupResult, error) {
					calls.Add(1)
					if !available.Load() {
						return 0, io.ErrUnexpectedEOF
					}
					return provider.RevocationQueued, nil
				}}
			return provider.Acquisition{Cleanup: resource}, io.ErrUnexpectedEOF
		})
		config := configurationFor(source, 0)
		var mu sync.Mutex
		var queued int
		config.Observe = func(event Event) {
			if event.Milestone != "provider_revocation_pending" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			queued++
			if event.ProviderRevocationConfirmed || event.ResourceID == "" {
				t.Error("handoff fabricated confirmation or lost identity")
			}
		}
		manager := newConfiguredManager(t, config)
		for i := 1; i <= MaximumOutstandingLeases; i++ {
			_, err := manager.Acquire(t.Context(), "primary", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{byte(i)}})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		if state := manager.Snapshot()[0]; state.ActiveLeases != 0 || state.PendingCleanup != MaximumOutstandingLeases {
			t.Fatal(state)
		}
		reconfigure(t, manager, accountConfig{})
		reconfigure(t, manager, configurationFor(newVersionedSource("replacement"), 0))
		if _, err := manager.Acquire(t.Context(), "primary", turnpath.Subject{}); !errors.Is(err, ErrCapacity) {
			t.Fatalf("configuration bypassed unresolved ownership: %v", err)
		}
		available.Store(true)
		time.Sleep(cleanupRetryDelay)
		synctest.Wait()
		if state := manager.Snapshot()[0]; state.PendingCleanup != 0 {
			t.Fatal(state)
		}
		if calls.Load() != 2*MaximumOutstandingLeases {
			t.Fatal(calls.Load())
		}
		mu.Lock()
		defer mu.Unlock()
		if queued != MaximumOutstandingLeases {
			t.Fatal(queued)
		}
	})
}

func TestCleanupExpiryAndBoundedShutdownNeverClaimRevocation(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "shutdown"}[shutdown], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lifetime := 2 * time.Second
				if shutdown {
					lifetime = time.Hour
				}
				source := cleanupSource(func(context.Context, provider.Subject) (provider.Acquisition, error) {
					return provider.Acquisition{Cleanup: &provider.Cleanup{
						ID: "unresolved", ExpiresAt: time.Now().Add(lifetime),
						Revoke: func(context.Context) (provider.CleanupResult, error) { return 0, io.ErrUnexpectedEOF },
					}}, io.ErrUnexpectedEOF
				})
				config := configurationFor(source, 0)
				var mu sync.Mutex
				var last Event
				config.Observe = func(event Event) {
					mu.Lock()
					defer mu.Unlock()
					last = event
					if event.ProviderRevocationConfirmed {
						t.Error("unreachable issuer reported revocation")
					}
				}
				manager, err := newAccountManager(config)
				if err != nil {
					t.Fatal(err)
				}
				_, err = manager.Acquire(t.Context(), "primary", turnpath.Subject{})
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
				synctest.Wait()
				if !shutdown {
					time.Sleep(lifetime)
					synctest.Wait()
				}
				start := time.Now()
				err = manager.Close()
				if shutdown {
					if err == nil || time.Since(start) != IssuanceTimeout ||
						manager.Snapshot()[0].PendingCleanup != 1 || len(manager.cleanups) != 1 {
						t.Fatalf("shutdown forgot unresolved cleanup: %v, %+v", err, manager.Snapshot())
					}
				} else if err != nil || manager.Snapshot()[0].PendingCleanup != 0 {
					t.Fatalf("expired authority retained capacity: %v", err)
				}
				mu.Lock()
				defer mu.Unlock()
				want := "provider_authority_expired"
				if shutdown {
					want = "provider_cleanup_unresolved"
				}
				if last.Milestone != want || last.ResourceID != "unresolved" {
					t.Fatal(last)
				}
			})
		})
	}
}
