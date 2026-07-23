package sessionruntime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestReceiverBlockLaneMarksOnlyPreAdmissionFailureReassignable(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	lease := id16[content.LeaseID](70)
	revisions := &receiverRevisionClient{leases: map[content.LeaseID]*remoteLeaseState{
		lease: {id: lease},
	}}
	lane := &receiverBlockLane{
		identity:  LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1},
		rpc:       newRPCClient(runtime, &deterministicReader{next: 71}),
		revisions: revisions,
	}
	demand := transfer.BlockDemand{LeaseID: lease}
	_, err := lane.FetchBlock(context.Background(), demand)
	capabilityType := reflect.TypeOf(transfer.NewDemandNotAdmitted(ErrLaneUnavailable))
	if reflect.TypeOf(err) != capabilityType || !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("pre-admission error=%v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = lane.FetchBlock(cancelled, demand)
	if !errors.Is(err, context.Canceled) || reflect.TypeOf(err) == capabilityType {
		t.Fatalf("cancelled fetch error=%v", err)
	}
}

type uncertainReceiverRequestChannel struct {
	*memoryChannel
	sends atomic.Int32
}

func (channel *uncertainReceiverRequestChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.sends.Add(1)
	if err := channel.memoryChannel.Send(ctx, frame); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (channel *uncertainReceiverRequestChannel) SendTerminal(
	ctx context.Context,
	frame framechannel.Frame,
) error {
	return channel.Send(ctx, frame)
}

type countingReceiverFallbackLane struct{ calls atomic.Int32 }

func (lane *countingReceiverFallbackLane) FetchBlock(
	context.Context,
	transfer.BlockDemand,
) (records.BlockRecord, error) {
	lane.calls.Add(1)
	return records.BlockRecord{}, errors.New("fallback was reached")
}

type signalingReceiverBlockLane struct {
	inner transfer.BlockLane
	done  chan struct{}
	once  sync.Once
}

func (lane *signalingReceiverBlockLane) FetchBlock(
	ctx context.Context,
	demand transfer.BlockDemand,
) (records.BlockRecord, error) {
	record, err := lane.inner.FetchBlock(ctx, demand)
	lane.once.Do(func() { close(lane.done) })
	return record, err
}

func TestReceiverBlockLaneUnknownSendDoesNotIssueSecondOperation(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	base, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	channel := &uncertainReceiverRequestChannel{memoryChannel: base}
	identity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	physical, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() { writerDone <- physical.writer.Run(t.Context()) }()
	// Cancellation deterministically selects the now-stopped attempted writer;
	// TryControl then performs synchronous local tombstone reconciliation.
	runtime.lanes.mu.Lock()
	runtime.lanes.next = 1
	runtime.lanes.mu.Unlock()

	share := id16[catalog.ShareInstance](72)
	fileID := id16[catalog.FileID](73)
	geometry, err := content.NewFileGeometry(uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share, fileID, id16[content.FileRevision](74), geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lease := id16[content.LeaseID](75)
	revisions := &receiverRevisionClient{leases: map[content.LeaseID]*remoteLeaseState{
		lease: {id: lease},
	}}
	reassemblyLimits := contentflow.ReassemblyLimits{Bytes: 1 << 20, Records: 8}
	processReassembly, _ := contentflow.NewReassemblyAccount("unknown-process", reassemblyLimits)
	shareReassembly, _ := contentflow.NewReassemblyAccount("unknown-share", reassemblyLimits)
	sessionReassembly, _ := contentflow.NewReassemblyAccount("unknown-session", reassemblyLimits)
	assembler, err := contentflow.NewAssembler(runtime.sessionID, contentflow.ReassemblyHierarchy{
		Process: processReassembly, Share: shareReassembly, Session: sessionReassembly,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blockLane := &receiverBlockLane{
		identity: identity, rpc: newRPCClient(runtime, &deterministicReader{next: 76}),
		assembler: assembler, revisions: revisions,
	}
	signalingLane := &signalingReceiverBlockLane{inner: blockLane, done: make(chan struct{})}
	contentLanes, err := transfer.NewLaneSet(transfer.LaneSetConfig{ProtocolSessionID: runtime.sessionID})
	if err != nil {
		t.Fatal(err)
	}
	defer contentLanes.Close()
	if err := contentLanes.Add(transfer.LaneIdentity{ID: 1, Epoch: 1}, signalingLane); err != nil {
		t.Fatal(err)
	}
	fallback := &countingReceiverFallbackLane{}
	if err := contentLanes.Add(transfer.LaneIdentity{ID: 2, Epoch: 1}, fallback); err != nil {
		t.Fatal(err)
	}
	processBudget, err := transfer.NewPlaintextBudget(uint64(catalog.MinChunkSize) * 2)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
		ShareInstance: share, Lanes: contentLanes, MaxBytes: uint64(catalog.MinChunkSize) * 2,
		ProcessBudget: processBudget, MaxConcurrentBlocks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, fetchErr := broker.GetBlock(ctx, lease, descriptor, 0)
	if fetchErr == nil || reflect.TypeOf(fetchErr) == reflect.TypeOf(transfer.NewDemandNotAdmitted(nil)) {
		t.Fatalf("unknown transport result=%v", fetchErr)
	}
	select {
	case <-signalingLane.done:
	case <-time.After(time.Second):
		t.Fatal("receiver block lane did not finish unknown-send reconciliation")
	}
	if channel.sends.Load() != 1 || fallback.calls.Load() != 0 {
		t.Fatalf("request sends=%d fallback calls=%d", channel.sends.Load(), fallback.calls.Load())
	}
	if runErr := <-writerDone; !errors.Is(runErr, io.ErrUnexpectedEOF) {
		t.Fatalf("writer error=%v", runErr)
	}
	blockLane.rpc.mu.Lock()
	activeCalls := len(blockLane.rpc.calls)
	blockLane.rpc.mu.Unlock()
	if activeCalls != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("calls=%d active=%d tombstones=%d", activeCalls, runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestReceiverBlockLaneProvenDropReassignsDemand(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	lease := id16[content.LeaseID](77)
	revisions := &receiverRevisionClient{leases: map[content.LeaseID]*remoteLeaseState{
		lease: {id: lease},
	}}
	blockLane := &receiverBlockLane{
		identity: LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1},
		rpc:      newRPCClient(runtime, &deterministicReader{next: 78}), revisions: revisions,
	}
	contentLanes, err := transfer.NewLaneSet(transfer.LaneSetConfig{ProtocolSessionID: runtime.sessionID})
	if err != nil {
		t.Fatal(err)
	}
	defer contentLanes.Close()
	if err := contentLanes.Add(transfer.LaneIdentity{ID: 1, Epoch: 1}, blockLane); err != nil {
		t.Fatal(err)
	}
	fallback := &countingReceiverFallbackLane{}
	if err := contentLanes.Add(transfer.LaneIdentity{ID: 2, Epoch: 1}, fallback); err != nil {
		t.Fatal(err)
	}

	share := id16[catalog.ShareInstance](79)
	geometry, _ := content.NewFileGeometry(uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	descriptor, _ := content.NewFileRevisionDescriptor(
		share, id16[catalog.FileID](80), id16[content.FileRevision](81), geometry, catalog.ModifiedTime{},
	)
	processBudget, _ := transfer.NewPlaintextBudget(uint64(catalog.MinChunkSize) * 2)
	broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
		ShareInstance: share, Lanes: contentLanes, MaxBytes: uint64(catalog.MinChunkSize) * 2,
		ProcessBudget: processBudget, MaxConcurrentBlocks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 0); err == nil {
		t.Fatal("fallback test unexpectedly returned a block")
	}
	if fallback.calls.Load() != 1 {
		t.Fatalf("proven drop fallback calls=%d", fallback.calls.Load())
	}
}

func TestRPCFailedBeginCancellationBudgetFailClosesSession(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 1}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	router, err := protocolsession.NewRoleRouter(protocolsession.RoleReceiver, operations)
	if err != nil {
		t.Fatal(err)
	}
	runtime.operations = operations
	runtime.router = router
	firstID := id16[protocolsession.OperationID](82)
	first, _ := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &firstID, []byte{0xa1, 0x00, 0x01},
	)
	firstAdmission, err := router.AdmitOutbound(first, protocolsession.OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := operations.CancelGeneration(firstAdmission.Generation); err != nil {
		t.Fatal(err)
	}
	secondID := id16[protocolsession.OperationID](83)
	second, _ := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &secondID, []byte{0xa1, 0x00, 0x02},
	)
	secondAdmission, err := router.AdmitOutbound(second, protocolsession.OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcileLocalCancel(secondAdmission.Generation); !errors.Is(err, protocolsession.ErrTombstoneBudget) {
		t.Fatalf("cleanup error=%v", err)
	}
	select {
	case <-runtime.ctx.Done():
	default:
		t.Fatal("tombstone exhaustion did not fail-close the session")
	}
	if !operations.Terminated() || operations.ActiveCount() != 0 || operations.TombstoneCount() != 0 {
		t.Fatalf("terminal=%v active=%d tombstones=%d", operations.Terminated(), operations.ActiveCount(), operations.TombstoneCount())
	}
}
