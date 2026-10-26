package receiversession

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"testing"
	"testing/synctest"
	"time"
)

type blockProtocolFixture struct {
	runtime  *sessioncore.Runtime
	recorder *protocolTraceRecorder
	lane     *receiverBlockLane
	demand   transferlanes.Demand
}

func newBlockProtocolFixture(t *testing.T, runWriter bool) blockProtocolFixture {
	t.Helper()
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	if runWriter {
		startReceiverPeerWriter(t, runtime)
	}
	geometry, err := content.NewFileGeometry(1, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		id16[catalog.ShareInstance](91), id16[catalog.FileID](61),
		id16[content.FileRevision](62), geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lease := id16[content.LeaseID](63)
	limits := contentflow.ReassemblyLimits{Bytes: 1 << 20, Records: 8}
	process, _ := contentflow.NewReassemblyAccount("trace-process", limits)
	share, _ := contentflow.NewReassemblyAccount("trace-share", limits)
	session, _ := contentflow.NewReassemblyAccount("trace-session", limits)
	assembler, err := contentflow.NewAssembler(runtime.ProtocolSessionID(), contentflow.ReassemblyHierarchy{
		Process: process, Share: share, Session: session,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lane := &receiverBlockLane{
		identity: runtimeIdentity(runtime), rpc: newTestRequests(runtime, &deterministicReader{next: 64}),
		assembler: assembler, revisions: &receiverRevisionClient{
			leases: map[content.LeaseID]*remoteLeaseState{lease: {id: lease}},
		},
	}
	if err := lane.rpc.Register(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lane.rpc.Close)
	return blockProtocolFixture{runtime, recorder, lane, transferlanes.Demand{LeaseID: lease, Descriptor: descriptor}}
}

type gatedBlockProtocolWinner struct{ ready <-chan struct{} }

func (lane gatedBlockProtocolWinner) FetchBlock(ctx context.Context, demand transferlanes.Demand, _ func(uint64)) (records.BlockRecord, error) {
	select {
	case <-lane.ready:
		return records.NewBlockRecord(demand.Descriptor, demand.Index, []byte{42})
	case <-ctx.Done():
		return records.BlockRecord{}, ctx.Err()
	}
}

func TestBlockProtocolTraceRaceLoserEndsNormally(t *testing.T) {
	for _, test := range []struct {
		name         string
		runWriter    bool
		beforeWinner func(blockProtocolFixture)
		wantCause    sessionobs.ProtocolOperationCause
	}{
		{"queued request", false, nil, sessionobs.ProtocolOperationCauseSuperseded},
		{"waiting for response", true, nil, sessionobs.ProtocolOperationCauseSuperseded},
		{"reassembly cleanup failure", true, func(fixture blockProtocolFixture) {
			fixture.lane.assembler.Close()
		}, sessionobs.ProtocolOperationCauseProtocolFailure},
		{"cancellation admission failure", true, func(fixture blockProtocolFixture) {
			trackedRequests(fixture.lane.rpc).cancelErr = sessioncore.ErrLaneUnavailable
		}, sessionobs.ProtocolOperationCauseLaneUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newBlockProtocolFixture(t, test.runWriter)
				lanes, err := transferlanes.New(transferlanes.Config{
					ProtocolSessionID:             fixture.runtime.ProtocolSessionID(),
					SettlementObservationCapacity: transferlanes.DefaultSettlementObservationCapacity,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(lanes.Close)
				winnerReady := make(chan struct{})
				if err := lanes.Add(transferlanes.Identity{ID: 1}, transferlanes.RouteDirect, fixture.lane); err != nil {
					t.Fatal(err)
				}
				if err := lanes.Add(transferlanes.Identity{ID: 2}, transferlanes.RouteDirect, gatedBlockProtocolWinner{winnerReady}); err != nil {
					t.Fatal(err)
				}
				budget, _ := transfer.NewPlaintextBudget(uint64(catalog.MinChunkSize))
				broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
					ShareInstance: fixture.demand.Descriptor.ShareInstance(), Blocks: lanes,
					MaxBytes: uint64(catalog.MinChunkSize), ProcessBudget: budget, MaxConcurrentBlocks: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := broker.Close(); err != nil {
						t.Error(err)
					}
				})
				done := make(chan error, 1)
				go func() {
					data, err := broker.GetBlock(context.Background(), fixture.demand.LeaseID, fixture.demand.Descriptor, 0)
					if err == nil && !bytes.Equal(data, []byte{42}) {
						t.Error("race changed the winning block")
					}
					done <- err
				}()
				// Hold the winner until the real RPC has reached its send/receive wait.
				// This covers both boundaries without network timing or wall-clock sleeps.
				synctest.Wait()
				call := onlyActiveCall(t, fixture.lane.rpc)
				// Advance the injected synctest clock until the stalled response is
				// eligible for rescue; healthy blocks never start duplicate probes.
				time.Sleep(time.Second)
				if test.beforeWinner != nil {
					test.beforeWinner(fixture)
				}
				close(winnerReady)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				lanes.Close()
				fixture.lane.rpc.Close()
				events := fixture.recorder.snapshot()
				stage := sessionobs.ProtocolOperationReceiverEnded
				if test.wantCause != sessionobs.ProtocolOperationCauseSuperseded {
					stage = sessionobs.ProtocolOperationReceiverFailed
				}
				if len(events) != 1 || events[0].OperationID != call.ID() ||
					events[0].Stage != stage || events[0].Cause != test.wantCause {
					t.Fatalf("race loser terminal = %+v", events)
				}
				for settlement := range lanes.SettlementObservations() {
					if settlement.FailedBlockAttempts != 0 || settlement.ReassignedBlocks != 0 || settlement.Incomplete {
						t.Fatalf("race settlement = %+v", settlement)
					}
				}
				assertRequestsRetired(t, fixture.lane.rpc)
			})
		})
	}
}

func TestBlockProtocolTraceRetainsUnownedCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "caller cancellation"
		if deadline {
			name = "first response inactivity"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newBlockProtocolFixture(t, true)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := fixture.lane.FetchBlock(ctx, fixture.demand, nil); done <- err }()
				synctest.Wait()
				if !deadline {
					cancel()
				}
				err := <-done
				wantErr, wantCause := context.Canceled, sessionobs.ProtocolOperationCauseCanceled
				if deadline {
					wantErr, wantCause = contentflow.ErrBlockResponseInactivity, sessionobs.ProtocolOperationCauseDeadline
				}
				events := fixture.recorder.snapshot()
				if !errors.Is(err, wantErr) || len(events) != 1 ||
					events[0].Stage != sessionobs.ProtocolOperationReceiverFailed || events[0].Cause != wantCause {
					t.Fatalf("error=%v terminal=%+v", err, events)
				}
			})
		})
	}
}

type gatedBlockProtocolOpener struct {
	RecordOpener
	started chan struct{}
	release <-chan struct{}
	err     error
}

func (opener gatedBlockProtocolOpener) OpenBlock(content.FileRevisionDescriptor, uint64, []byte) (records.BlockRecord, error) {
	close(opener.started)
	<-opener.release
	return records.BlockRecord{}, opener.err
}

func TestBlockProtocolTraceWaitsForDecodingAfterRPCShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newBlockProtocolFixture(t, true)
		decodeFailure := errors.New("authenticated block failed local validation")
		started, release := make(chan struct{}), make(chan struct{})
		fixture.lane.opener = gatedBlockProtocolOpener{
			started: started, release: release, err: errors.Join(context.Canceled, decodeFailure),
		}
		done := make(chan error, 1)
		go func() { _, err := fixture.lane.FetchBlock(context.Background(), fixture.demand, nil); done <- err }()
		synctest.Wait()
		call := onlyActiveCall(t, fixture.lane.rpc)
		fragments, err := contentflow.FragmentRecord(call.ID(), []byte{1})
		if err != nil {
			t.Fatal(err)
		}
		if err := deliverCallResponse(call, fragments[0]); err != nil {
			t.Fatal(err)
		}
		<-started
		fixture.lane.rpc.Close()
		if events := fixture.recorder.snapshot(); len(events) != 0 {
			t.Fatalf("RPC shutdown published before block decoding joined: %+v", events)
		}
		close(release)
		if err := <-done; !errors.Is(err, decodeFailure) {
			t.Fatalf("decode failure lost: %v", err)
		}
		events := fixture.recorder.snapshot()
		if len(events) != 1 || events[0].Stage != sessionobs.ProtocolOperationReceiverFailed ||
			events[0].Cause != sessionobs.ProtocolOperationCauseRuntimeClosed {
			t.Fatalf("joined shutdown terminal = %+v", events)
		}
	})
}

func TestBlockProtocolTraceRejectsMalformedFinalResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newBlockProtocolFixture(t, true)
		done := make(chan error, 1)
		go func() { _, err := fixture.lane.FetchBlock(context.Background(), fixture.demand, nil); done <- err }()
		synctest.Wait()
		call := onlyActiveCall(t, fixture.lane.rpc)
		message, err := protocolsession.NewMessage(protocolsession.MessageOperationComplete, new(call.ID()), []byte{0xa0})
		if err != nil {
			t.Fatal(err)
		}
		if err := deliverCallResponse(call, message); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatal("malformed final was accepted")
		}
		events := fixture.recorder.snapshot()
		if len(events) != 1 || !events[0].HasResponse || events[0].ResponseKind != protocolsession.MessageOperationComplete ||
			events[0].Stage != sessionobs.ProtocolOperationReceiverFailed || events[0].Cause != sessionobs.ProtocolOperationCauseProtocolFailure {
			t.Fatalf("malformed final terminal = %+v", events)
		}
	})
}
