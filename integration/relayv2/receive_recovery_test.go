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

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/osfs"
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
	afterWrite       func()
}

func (a countedAuthority) SelectOperation(ctx context.Context, s transfer.SelectionSpec, selector receivecontract.OperationSelection) (engine.OutputLookup, error) {
	a.lookups.Add(1)
	lookup, err := a.OutputAuthority.SelectOperation(ctx, s, selector)
	if lookup.Reservation != nil {
		lookup.Reservation = countedReservation{lookup.Reservation, a.creates, a.afterWrite}
	}
	return lookup, err
}

type countedReservation struct {
	engine.OutputReservation
	count      *atomic.Int32
	afterWrite func()
}

func (r countedReservation) Create(ctx context.Context, s receivecontract.ArtifactSpec) (engine.OutputOperation, error) {
	r.count.Add(1)
	operation, err := r.OutputReservation.Create(ctx, s)
	if err != nil {
		return operation, err
	}
	materializer := operation.Materializer
	operation.Materializer = transfer.DirectTreeMaterializerFunc(func(ctx context.Context, intent transfer.ReceiveIntent) (transfer.DirectTreeSession, error) {
		session, err := materializer.OpenDirectTree(ctx, intent)
		if err != nil {
			return nil, err
		}
		return disconnectingTree{session, r.afterWrite}, nil
	})
	return operation, nil
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
	connections := make(chan engine.ReceiveConnection, 8)
	clock := &recoveryClock{}
	var dials, primaryAttempts, stoppedAttempts, lookups, creates, authorities atomic.Int32
	application, err := engine.New(engine.Config{Receive: engine.ReceiveDependencies{
		Recovery: engine.ReceiveRecoverySettings{Clock: clock, Jitter: func(d time.Duration) time.Duration { return d }},
		ReceiverDial: func(ctx context.Context, config engine.ReceiveDialRequest) (engine.ReceiveConnection, error) {
			if config.RelayBaseURL == stoppedRelayURL {
				stoppedAttempts.Add(1)
				return nil, &relayv2.RelayError{Code: v2.ErrorStopped}
			}
			if primaryAttempts.Add(1) > 1 && clock.Now().Before(time.Unix(181, 0)) {
				return nil, errors.New("injected endpoint outage")
			}
			shareID, err := v2.ShareIDFromBytes(config.ShareID)
			if err != nil {
				return nil, err
			}
			connection, err := relayset.DialReceiver(ctx, relayv2.ReceiverConfig{RelayBaseURL: config.RelayBaseURL, ShareID: shareID})
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
	candidates := make([]engine.ShareRelayCandidate, 0, len(relayURLs))
	for _, address := range relayURLs {
		candidates = append(candidates, engine.ShareRelayCandidate{PublicURL: address})
	}
	sender, err := application.StartShare(ctx, engine.ShareRequest{Source: liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{Paths: []string{filename}, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity)})
	}), Relays: candidates, ChunkSize: catalog.MinChunkSize})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := sender.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
		authority, err := (filesystem.FilesystemOutput{RootPath: root.RootPath, CreateRoot: true}).NewOutputAuthority(config)
		if err != nil {
			return nil, err
		}
		return countedAuthority{OutputAuthority: authority, lookups: &lookups, creates: &creates, afterWrite: func() { cut.Do(func() { connection := <-connections; _ = connection.Channel().Close() }) }}, nil
	})
	diagnosticsStream, err := diagnostics.NewStream(4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnosticsStream.Close()
	receiver, err := application.StartReceive(ctx, engine.ReceiveRequest{Capability: ready.Capability, Connectivity: engine.ConnectivityRelayOnly, Output: factory, Destination: root.RootPath, Diagnostics: diagnosticsStream.Sink()})
	if err != nil {
		t.Fatal(err)
	}
	var events []engine.Event
	for value := range receiver.Observations() {
		events = append(events, value.Event)
		switch event := value.Event.(type) {
		case appobs.RelayRecovering:
			trace.RequireRecord(t, "receiver_recovery_observed", testrun.OutcomeSucceeded, event)
		case appobs.EngineTaskObserved:
			trace.RequireRecord(t, "receiver_generation_changed", testrun.OutcomeSucceeded, event)
		}
	}
	result, err := receiver.Wait(context.Background())
	if err != nil || result.Outcome != engine.OutcomeSuccess {
		for _, event := range events {
			switch value := event.(type) {
			case appobs.RelayRecovering, appobs.EngineTaskObserved:
				t.Logf("%T: %+v", value, value)

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
	jobs := map[string]bool{}
	sessions := map[string]bool{}
	for _, event := range events {
		switch value := event.(type) {
		case appobs.RelayRecovering:
			waited = waited || value.State() == appobs.RelayRecoveryWaiting
			restored = restored || (value.State() == appobs.RelayRecoverySucceeded && value.Attempt() > 1)

		}
	}
	diagnosticsStream.Close()
	for record := range diagnosticsStream.Records() {
		if record.Name() == "transfer_lifecycle" {
			jobs[record.Correlation().Get("transfer_job_id").Text()] = true
			sessions[record.Correlation().Get("protocol_session_id").Text()] = true
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

// Fault injection belongs at the output boundary; diagnostic consumers never
// control the transfer or hold a producer goroutine open.
type disconnectingTree struct {
	transfer.DirectTreeSession
	afterWrite func()
}

func (tree disconnectingTree) BeginFile(ctx context.Context, file transfer.MaterializationFile) (transfer.FileStart, error) {
	start, err := tree.DirectTreeSession.BeginFile(ctx, file)
	if err != nil {
		return start, err
	}
	transaction, durable, active := start.Transaction()
	if !active {
		return start, nil
	}
	return transfer.NewFileTransactionStart(disconnectingTransaction{transaction, tree.afterWrite}, durable)
}

type disconnectingTransaction struct {
	transfer.FileTransaction
	afterWrite func()
}

func (transaction disconnectingTransaction) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	err := transaction.FileTransaction.WriteRange(ctx, offset, data)
	if err == nil {
		transaction.afterWrite()
	}
	return err
}
