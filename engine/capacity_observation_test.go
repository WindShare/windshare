package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/liveshare"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestConcurrentShareCapacityObservationsKeepTheirTaskAndSharedBudget(t *testing.T) {
	configurations := make(chan liveshare.SenderConfig, 2)
	limits := content.CapacityLimits{StableHandles: 1, ActiveLeases: 2}
	application := newTestEngine(t, Config{
		RevisionCapacity: content.HandlePoolConfig{Limits: limits},
		Share: ShareDependencies{
			Prepare: func(_ context.Context, config liveshare.SenderConfig) (PreparedShare, error) {
				configurations <- config
				return &preparedFacadeShare{}, nil
			},
			Peers:       func() (SharePeers, error) { return &facadePeers{}, nil },
			Relays:      func(context.Context, []ShareRelayCandidate) (ShareRelays, error) { return &facadeRelays{}, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), shareStartupTestTimeout)
	defer cancel()
	stream, _ := diagnostics.NewStream(256, nil)
	var tasks [2]*ShareTask
	var stores [2]*content.RevisionStore
	var sessions [2]*content.RevisionSession
	var files [2]catalog.FileID
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
		current, err := application.StartShare(ctx, ShareRequest{Diagnostics: stream.Sink(), RelayIntent: relayprofile.UseExplicit(
			[]relayprofile.
				Candidate{{PublicURL: "https://relay.example"}}),
		})
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
		stores[i], files[i], err = newEngineTestRevisionStore(configs[i].RevisionCapacity, byte(i+1), limits, configs[i].CapacityTracer)
		if err != nil {
			t.Fatal(err)
		}
		raw := make([]byte, appobs.IdentityBytes)
		raw[0] = byte(i + 1)
		sessions[i], err = stores[i].RegisterSession(content.SessionConfig{
			SessionID: content.SessionID(base64.RawURLEncoding.EncodeToString(raw)), Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if configs[0].RevisionCapacity != configs[1].RevisionCapacity {
		t.Fatal("sharing split the aggregate resource budget")
	}
	type admission struct {
		index int
		lease content.RevisionLease
		err   error
	}
	results := make(chan admission, len(tasks))
	start := make(chan struct{})
	for i := range tasks {
		go func() {
			<-start
			lease, err := sessions[i].OpenRevision(ctx, files[i])
			results <- admission{index: i, lease: lease, err: err}
		}()
	}
	close(start)
	granted, denied := 0, 0
	var held []admission
	for range tasks {
		select {
		case result := <-results:
			if result.err == nil {
				granted++
				// Retain the open lease until both contenders return.
				held = append(held, result)
			} else {
				var busy *content.CapacityBusyError
				if !errors.As(result.err, &busy) || busy.Scope() != content.CapacityScopeProcess {
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
	for _, open := range held {
		if err := sessions[open.index].EndLease(open.lease.ID(), content.LeaseRelinquished); err != nil {
			t.Fatal(err)
		}
	}
	// Closing an observer's task must not replace or revoke the other share's
	// publication authority. The terminal reservations are independent resources.
	tasks[0].StopShare()
	if _, err := tasks[0].Wait(ctx); err != nil {
		t.Fatal(err)
	}
	afterLease, err := sessions[1].OpenRevision(ctx, files[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions[1].EndLease(afterLease.ID(), content.LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	tasks[1].StopShare()
	if _, err := tasks[1].Wait(ctx); err != nil {
		t.Fatal(err)
	}

	stream.Close()
	counts := map[string]int{}
	for record := range stream.Records() {
		if record.Name() != "sender_capacity" {
			continue
		}
		counts[record.Scope().TaskID]++
		index := -1
		for i, current := range tasks {
			if string(current.ID()) == record.Scope().TaskID {
				index = i
			}
		}
		if index < 0 {
			t.Fatalf("unknown task: %+v", record.Scope())
		}
		f := record.Payload()
		if f.Get("share_id").Text() != stores[index].CapacitySnapshot().Share().Identity() {
			t.Fatal("capacity event crossed share ownership")
		}
		if _, err := json.Marshal(f); err != nil {
			t.Fatal(err)
		}
	}
	for i, current := range tasks {
		if counts[string(current.ID())] != i+1 {
			t.Fatalf("share %d diagnostic count: %v", i, counts)
		}
		for observation := range current.Observations() {
			if _, ok := observation.Event.(appobs.ObserverLossObserved); ok {
				t.Fatal("source diagnostic entered product projection")
			}
		}
	}
}
