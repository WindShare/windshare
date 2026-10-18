package relayv2_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const recoveryIntegrationTimeout = 30 * time.Second

type recoveryClock struct{ elapsed atomic.Int64 }

func (c *recoveryClock) Now() time.Time { return time.Unix(1, c.elapsed.Load()) }
func (c *recoveryClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.elapsed.Add(int64(d))
	return nil
}

type countedAuthority struct {
	engine.OutputAuthority
	lookups, creates *atomic.Int32
}

func (a countedAuthority) LookupActive(ctx context.Context, s transfer.SelectionSpec) (engine.OutputLookup, error) {
	a.lookups.Add(1)
	lookup, err := a.OutputAuthority.LookupActive(ctx, s)
	if lookup.Reservation != nil {
		lookup.Reservation = countedReservation{lookup.Reservation, a.creates}
	}
	return lookup, err
}

type countedReservation struct {
	engine.OutputReservation
	count *atomic.Int32
}

func (r countedReservation) Create(ctx context.Context, s receivecontract.ArtifactSpec) (engine.OutputOperation, error) {
	r.count.Add(1)
	return r.OutputReservation.Create(ctx, s)
}

func TestGetReplacesLostSessionAndKeepsOneJobAndOutputReservation(t *testing.T) {
	if testing.Short() {
		t.Skip("native receive recovery belongs to the long integration gate")
	}
	filename := filepath.Join(t.TempDir(), "file.bin")
	root := testoutputroot.New(t)
	trace, relayURLs := startNativeRelays(t, 2)
	stoppedRelayURL := relayURLs[1]
	payload := bytes.Repeat([]byte("preserved output"), 4096)
	if err := os.WriteFile(filename, payload, 0600); err != nil {
		t.Fatal(err)
	}
	connections := make(chan *relayv2.ReceiverConnection, 8)
	clock := &recoveryClock{}
	var dials, primaryAttempts, stoppedAttempts, lookups, creates, authorities atomic.Int32
	application, err := engine.New(engine.Config{Receive: engine.ReceiveDependencies{
		Recovery: relayset.ReceiverRecoveryOptions{Clock: clock, Jitter: func(d time.Duration) time.Duration { return d }},
		ReceiverDial: func(ctx context.Context, config relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
			if config.RelayBaseURL == stoppedRelayURL {
				stoppedAttempts.Add(1)
				return nil, &relayv2.RelayError{Code: v2.ErrorStopped}
			}
			if primaryAttempts.Add(1) > 1 && clock.Now().Before(time.Unix(181, 0)) {
				return nil, errors.New("injected endpoint outage")
			}
			connection, err := relayv2.DialReceiver(ctx, config)
			if err == nil {
				dials.Add(1)
				connections <- connection
			}
			return connection, err
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "receive engine", application.Close)
	// Retry timing is virtual, but authenticated sessions and native disk writes
	// share CI resources. This watchdog bounds hangs, not recovery latency.
	ctx, cancel := context.WithTimeout(t.Context(), recoveryIntegrationTimeout)
	defer cancel()
	sender, err := application.StartShare(ctx, engine.ShareRequest{Source: liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{Paths: []string{filename}, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity)})
	}), RelayURLs: relayURLs, ChunkSize: catalog.MinChunkSize})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := sender.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sender.Activate(nil)
	trace.RequireCleanup(t, "sender share", func(cleanup context.Context) error {
		sender.StopShare()
		result, err := sender.Wait(cleanup)
		if err != nil || result.FailureClass != engine.FailureNone {
			return fmt.Errorf("sender settlement=%+v wait=%v", result, err)
		}
		return nil
	})
	var cut sync.Once
	factory := engine.OutputFactoryFunc(func(config engine.OutputConfig) (engine.OutputAuthority, error) {
		authorities.Add(1)
		trace := config.Tracer
		config.Tracer = osfs.FilesystemOutputTraceFunc(func(value osfs.FilesystemOutputTrace) {
			if trace != nil {
				trace.TraceFilesystemOutput(value)
			}
			// Cut at a native write boundary, independently of observation consumption.
			if value.RuntimeOperation == osfs.FilesystemOutputRuntimeWriteRange && !value.Failed {
				cut.Do(func() { connection := <-connections; _ = connection.Channel().Close() })
			}
		})
		authority, err := (engine.FilesystemOutput{RootPath: root.RootPath, CreateRoot: true}).NewOutputAuthority(config)
		if err != nil {
			return nil, err
		}
		return countedAuthority{OutputAuthority: authority, lookups: &lookups, creates: &creates}, nil
	})
	receiver, err := application.StartReceive(ctx, engine.ReceiveRequest{Capability: ready.Capability, Connectivity: engine.ConnectivityRelayOnly, Output: factory, Destination: root.RootPath, Diagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	var events []engine.Event
	for value := range receiver.Observations() {
		events = append(events, value.Event)
		switch event := value.Event.(type) {
		case engine.ReceiveRecoveryObserved:
			trace.RequireRecord(t, "receiver_recovery_observed", testrun.OutcomeSucceeded, event)
		case engine.ReceiveGenerationChanged:
			trace.RequireRecord(t, "receiver_generation_changed", testrun.OutcomeSucceeded, event)
		}
	}
	result, err := receiver.Wait(context.Background())
	if err != nil || result.Outcome != engine.OutcomeSuccess {
		for _, event := range events {
			switch value := event.(type) {
			case engine.ReceiveRecoveryObserved, engine.ReceiveGenerationChanged, engine.ReceiveAdmissionObserved:
				t.Logf("%T: %+v", value, value)
			case engine.ReceiveProtocolObserved:
				t.Logf("protocol at=%s %T: %+v", value.Value.ObservedAt().Format(time.RFC3339Nano), value.Value, value.Value)
			case engine.ReceiveTransferObserved:
				trace := value.Value
				t.Logf("transfer session=%x job=%x stage=%v verified=%d published=%d",
					trace.ProtocolSessionID, trace.TransferJobID, trace.Stage, trace.Progress.VerifiedBytes, trace.Progress.PublishedBytes)
			}
		}
		t.Fatalf("outcome=%v failure=%v cause=%v test_context=%v progress=%+v wait=%v",
			result.Outcome, result.FailureClass, result.Err, ctx.Err(), result.Value.Transfer.Progress, err)
	}
	if dials.Load() != 2 || primaryAttempts.Load() < 4 || stoppedAttempts.Load() != 1 {
		t.Fatalf("generations=%d attempts=%d stopped=%d", dials.Load(), primaryAttempts.Load(), stoppedAttempts.Load())
	}
	if authorities.Load() != 1 || lookups.Load() != 1 || creates.Load() != 1 {
		t.Fatalf("operation resources authorities=%d lookups=%d creates=%d", authorities.Load(), lookups.Load(), creates.Load())
	}
	actual, err := os.ReadFile(filepath.Join(root.RootPath, "file.bin"))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("output bytes=%d err=%v", len(actual), err)
	}
	waited, restored := false, false
	jobs := map[transfer.TransferJobID]bool{}
	sessions := map[protocolsession.ProtocolSessionID]bool{}
	for _, event := range events {
		switch value := event.(type) {
		case engine.ReceiveRecoveryObserved:
			waited = waited || value.Value.Phase == relayset.ReceiverRecoveryWaiting
			restored = restored || (value.Value.Phase == relayset.ReceiverRecoveryConnected && value.Value.Attempt > 1)
		case engine.ReceiveTransferObserved:
			jobs[value.Value.TransferJobID] = true
			sessions[value.Value.ProtocolSessionID] = true
		}
	}
	if !waited || !restored || len(jobs) != 1 || len(sessions) != 2 {
		t.Fatalf("waiting=%v restored=%v jobs=%d sessions=%d", waited, restored, len(jobs), len(sessions))
	}
	if result.Value.Transfer.Progress.VerifiedBytes != uint64(len(payload)) {
		t.Fatalf("verified progress=%+v", result.Value.Transfer.Progress)
	}
	trace.RequireSuccess(t)
}
