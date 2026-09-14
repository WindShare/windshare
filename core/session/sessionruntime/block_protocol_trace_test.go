package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type blockProtocolFixture struct {
	runtime  *runtimeCore
	recorder *protocolTraceRecorder
	lane     *receiverBlockLane
	demand   transfer.BlockDemand
}

func newBlockProtocolFixture(t *testing.T, runWriter bool) blockProtocolFixture {
	t.Helper()
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	selected, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	if runWriter {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- selected.writer.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
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
	assembler, err := contentflow.NewAssembler(runtime.sessionID, contentflow.ReassemblyHierarchy{
		Process: process, Share: share, Session: session,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lane := &receiverBlockLane{
		identity: runtime.initial, rpc: newRPCClient(runtime, &deterministicReader{next: 64}),
		assembler: assembler, revisions: &receiverRevisionClient{
			leases: map[content.LeaseID]*remoteLeaseState{lease: {id: lease}},
		},
	}
	t.Cleanup(lane.rpc.Close)
	return blockProtocolFixture{runtime, recorder, lane, transfer.BlockDemand{LeaseID: lease, Descriptor: descriptor}}
}

type gatedBlockProtocolWinner struct{ ready <-chan struct{} }

func (lane gatedBlockProtocolWinner) FetchBlock(ctx context.Context, demand transfer.BlockDemand) (records.BlockRecord, error) {
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
		wantCause    ProtocolOperationCause
	}{
		{"queued request", false, nil, ProtocolOperationCauseSuperseded},
		{"waiting for response", true, nil, ProtocolOperationCauseSuperseded},
		{"reassembly cleanup failure", true, func(fixture blockProtocolFixture) {
			fixture.lane.assembler.Close()
		}, ProtocolOperationCauseProtocolFailure},
		{"cancellation admission failure", true, func(fixture blockProtocolFixture) {
			selected, _ := fixture.runtime.lanes.selectLane(&fixture.runtime.initial)
			fixture.runtime.lanes.markSelectedClosing(selected)
		}, ProtocolOperationCauseLaneUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newBlockProtocolFixture(t, test.runWriter)
				lanes, err := transfer.NewLaneSet(transfer.LaneSetConfig{
					ProtocolSessionID: fixture.runtime.sessionID, RaceWidth: 2,
					SettlementObservationCapacity: transfer.DefaultLaneSettlementObservationCapacity,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(lanes.Close)
				winnerReady := make(chan struct{})
				if err := lanes.Add(transfer.LaneIdentity{ID: 1}, transfer.LaneRouteRelay, fixture.lane); err != nil {
					t.Fatal(err)
				}
				if err := lanes.Add(transfer.LaneIdentity{ID: 2}, transfer.LaneRouteDirect, gatedBlockProtocolWinner{winnerReady}); err != nil {
					t.Fatal(err)
				}
				budget, _ := transfer.NewPlaintextBudget(uint64(catalog.MinChunkSize))
				broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
					ShareInstance: fixture.demand.Descriptor.ShareInstance(), Lanes: lanes,
					MaxBytes: uint64(catalog.MinChunkSize), ProcessBudget: budget, MaxConcurrentBlocks: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(broker.Close)
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
				stage := ProtocolOperationReceiverEnded
				if test.wantCause != ProtocolOperationCauseSuperseded {
					stage = ProtocolOperationReceiverFailed
				}
				if len(events) != 1 || events[0].OperationID != call.id ||
					events[0].Stage != stage || events[0].Cause != test.wantCause {
					t.Fatalf("race loser terminal = %+v", events)
				}
				for settlement := range lanes.SettlementObservations() {
					if settlement.FailedBlockAttempts != 0 || settlement.ReassignedBlocks != 0 || settlement.Incomplete {
						t.Fatalf("race settlement = %+v", settlement)
					}
				}
				if fixture.runtime.operations.ActiveCount() != 0 {
					t.Fatal("race loser retained active protocol authority")
				}
			})
		})
	}
}

func TestBlockProtocolTraceRetainsUnownedCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "caller cancellation"
		if deadline {
			name = "fragment inactivity"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newBlockProtocolFixture(t, true)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := fixture.lane.FetchBlock(ctx, fixture.demand); done <- err }()
				synctest.Wait()
				if !deadline {
					cancel()
				}
				err := <-done
				wantErr, wantCause := context.Canceled, ProtocolOperationCauseCanceled
				if deadline {
					wantErr, wantCause = contentflow.ErrFragmentInactivity, ProtocolOperationCauseDeadline
				}
				events := fixture.recorder.snapshot()
				if !errors.Is(err, wantErr) || len(events) != 1 ||
					events[0].Stage != ProtocolOperationReceiverFailed || events[0].Cause != wantCause {
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
		go func() { _, err := fixture.lane.FetchBlock(context.Background(), fixture.demand); done <- err }()
		synctest.Wait()
		call := onlyActiveCall(t, fixture.lane.rpc)
		fragments, err := contentflow.FragmentRecord(call.id, []byte{1})
		if err != nil {
			t.Fatal(err)
		}
		generation, _ := call.operationAuthority()
		if err := call.enqueue(operationResponse{message: fragments[0], generation: generation}); err != nil {
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
		if len(events) != 1 || events[0].Stage != ProtocolOperationReceiverFailed ||
			events[0].Cause != ProtocolOperationCauseRuntimeClosed {
			t.Fatalf("joined shutdown terminal = %+v", events)
		}
	})
}

func TestProtocolOperationCauseRetainsJoinedFaults(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want ProtocolOperationCause
	}{
		{"canceled cleanup", errors.Join(context.Canceled, errors.New("cleanup failed")), ProtocolOperationCauseProtocolFailure},
		{"wrapped canceled cleanup", fmt.Errorf("operation: %w", errors.Join(errors.New("cleanup failed"), context.Canceled)), ProtocolOperationCauseProtocolFailure},
		{"canceled runtime", errors.Join(context.Canceled, ErrRuntimeClosed), ProtocolOperationCauseRuntimeClosed},
		{"canceled deadline", errors.Join(context.Canceled, context.DeadlineExceeded), ProtocolOperationCauseDeadline},
		{"unproven missing authority", ErrOperationMissing, ProtocolOperationCauseOperationClosed},
		{"opaque cancellation match", cancellationMatchingBlockFault{}, ProtocolOperationCauseProtocolFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := newOperationCall(id16[protocolsession.OperationID](65), protocolsession.MessageRequestBlocks, time.Now(), 0, false, true)
			owner := call.claimProtocolTermination()
			call.recordProtocolTraceFailure(test.err)
			event, ok := call.protocolOperationTerminationTrace(time.Now(), owner,
				protocolOperationOutcome{ProtocolOperationReceiverEnded, ProtocolOperationCauseSuperseded})
			if !ok || event.Stage != ProtocolOperationReceiverFailed || event.Cause != test.want {
				t.Fatalf("joined cancellation terminal = %+v present=%v", event, ok)
			}
		})
	}
}

type cancellationMatchingBlockFault struct{}

func (cancellationMatchingBlockFault) Error() string        { return "fault with a cancellation match" }
func (cancellationMatchingBlockFault) Is(target error) bool { return target == context.Canceled }

func TestBlockProtocolTraceRejectsMalformedFinalResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newBlockProtocolFixture(t, true)
		done := make(chan error, 1)
		go func() { _, err := fixture.lane.FetchBlock(context.Background(), fixture.demand); done <- err }()
		synctest.Wait()
		call := onlyActiveCall(t, fixture.lane.rpc)
		message, err := protocolsession.NewMessage(protocolsession.MessageOperationComplete, &call.id, []byte{0xa0})
		if err != nil {
			t.Fatal(err)
		}
		generation, _ := call.operationAuthority()
		if err := call.enqueue(operationResponse{message: message, generation: generation}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatal("malformed final was accepted")
		}
		events := fixture.recorder.snapshot()
		if len(events) != 1 || !events[0].HasResponse || events[0].ResponseKind != protocolsession.MessageOperationComplete ||
			events[0].Stage != ProtocolOperationReceiverFailed || events[0].Cause != ProtocolOperationCauseProtocolFailure {
			t.Fatalf("malformed final terminal = %+v", events)
		}
	})
}
