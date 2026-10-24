package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
	"github.com/windshare/windshare/engine/relayprofile"
)

type capacityReclaimTarget struct{}

func (capacityReclaimTarget) ReclaimIdle(_ context.Context, claim revisioncapacity.ReclaimClaim) revisioncapacity.ReclaimResult {
	return revisioncapacity.ReclaimCompleted(claim, nil)
}

func TestConcurrentShareCapacityObservationsKeepTheirTaskAndSharedBudget(t *testing.T) {
	configurations := make(chan liveshare.SenderConfig, 2)
	limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 2}
	application := newTestEngine(t, Config{
		RevisionCapacity: revisioncapacity.ProcessConfig{Limits: limits},
		Share: ShareDependencies{
			Prepare: func(_ context.Context, config liveshare.SenderConfig) (PreparedShare, error) {
				configurations <- config
				return &preparedFacadeShare{}, nil
			},
			Peers:       func() (SharePeers, error) { return &facadePeers{}, nil },
			Relays:      func(context.Context, []relayprofile.Candidate) (ShareRelays, error) { return &facadeRelays{}, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), shareStartupTestTimeout)
	defer cancel()
	var tasks [2]*ShareTask
	var stores [2]*revisioncapacity.StoreRegistration
	var sessions [2]*revisioncapacity.SessionRegistration
	t.Cleanup(func() {
		for i := range stores {
			if sessions[i] != nil {
				if err := sessions[i].Close(); err != nil {
					t.Error(err)
				}
			}
			if stores[i] != nil {
				if err := stores[i].Close(); err != nil {
					t.Error(err)
				}
			}
		}
	})
	var configs [2]liveshare.SenderConfig
	for i := range tasks {
		current, err := application.StartShare(ctx, ShareRequest{Diagnostics: true})
		if err != nil {
			t.Fatal(err)
		}
		tasks[i] = current
		if _, err := current.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		configs[i] = <-configurations
		if configs[i].CapacityTracer == nil {
			t.Fatal("share has no capacity observer")
		}
		storeID := revisioncapacity.StoreID([]string{"store-a", "store-b"}[i])
		stores[i], err = configs[i].RevisionCapacity.RegisterStore(revisioncapacity.StoreConfig{
			StoreID: storeID, ShareID: revisioncapacity.ShareID(storeID), Limits: limits, Tracer: configs[i].CapacityTracer,
		}, capacityReclaimTarget{})
		if err != nil {
			t.Fatal(err)
		}
		raw := make([]byte, appobs.IdentityBytes)
		raw[0] = byte(i + 1)
		sessions[i], err = stores[i].RegisterSession(revisioncapacity.SessionConfig{
			SessionID: revisioncapacity.SessionID(base64.RawURLEncoding.EncodeToString(raw)), Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if configs[0].RevisionCapacity != configs[1].RevisionCapacity {
		t.Fatal("sharing split the aggregate resource budget")
	}
	type admission struct {
		grant revisioncapacity.AdmissionGrant
		err   error
	}
	results := make(chan admission, len(tasks))
	start := make(chan struct{})
	for i := range tasks {
		go func() {
			<-start
			grant, err := stores[i].Admit(ctx, revisioncapacity.AdmissionRequest{Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "revision", Session: sessions[i]})
			results <- admission{grant, err}
		}()
	}
	close(start)
	granted, denied := 0, 0
	var grants []revisioncapacity.AdmissionGrant
	for range tasks {
		select {
		case result := <-results:
			if result.err == nil {
				granted++
				// Retain the provisional reservation until both contenders return.
				grants = append(grants, result.grant)
			} else {
				var busy *revisioncapacity.CapacityBusyError
				if !errors.As(result.err, &busy) || busy.Scope() != revisioncapacity.CapacityScopeProcess {
					t.Fatalf("unexpected admission failure: %v", result.err)
				}
				denied++
			}
		case <-ctx.Done():
			t.Fatal("undrained observation consumers blocked capacity admission")
		}
	}
	if granted != 1 || denied != 1 {
		t.Fatalf("shared budget granted=%d denied=%d", granted, denied)
	}
	for _, grant := range grants {
		if err := grant.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	// Closing an observer's task must not replace or revoke the other share's
	// publication authority. The terminal reservations are independent resources.
	tasks[0].StopShare()
	if _, err := tasks[0].Wait(ctx); err != nil {
		t.Fatal(err)
	}
	grant, err := stores[1].Admit(ctx, revisioncapacity.AdmissionRequest{Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "after-stop", Session: sessions[1]})
	if err != nil {
		t.Fatal(err)
	}
	if err := grant.Abort(); err != nil {
		t.Fatal(err)
	}
	tasks[1].StopShare()
	if _, err := tasks[1].Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for i, current := range tasks {
		capacityFacts, rejections := 0, 0
		for envelope := range current.Observations() {
			if envelope.TaskID != current.ID() {
				t.Fatalf("task attribution: %+v", envelope)
			}
			switch fact := envelope.Event.(type) {
			case appobs.SenderCapacityObserved:
				capacityFacts++
				if fact.CapacityStoreID().String() != string(stores[i].StoreID()) || fact.CapacityShareID().String() != string(stores[i].ShareID()) {
					t.Fatalf("capacity event crossed share ownership: %+v", fact)
				}
				encoded, err := wire.Encode(fact)
				if err != nil {
					t.Fatal(err)
				}
				payload := encoded.Payload.(wire.SenderCapacityPayload)
				if payload.CapacityStoreID != string(stores[i].StoreID()) || payload.CapacityShareID != string(stores[i].ShareID()) {
					t.Fatalf("serialized capacity lost owner: %+v", payload)
				}
			case appobs.ObserverLossObserved:
				rejections++
			}
		}
		if capacityFacts != i+1 || rejections != 0 {
			t.Fatalf("share %d capacity=%d rejection=%d", i, capacityFacts, rejections)
		}
	}

}
