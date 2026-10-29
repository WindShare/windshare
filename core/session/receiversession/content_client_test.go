package receiversession

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestBlockFragmentInactivityIsALaneTransportBoundary(t *testing.T) {
	err := blockTransportBoundary(contentflow.ErrFragmentInactivity)
	var boundary *transferfault.BoundaryError
	expected, buildErr := transferfault.NewSession(transferfault.ScopeOutputPause, transferfault.SessionTransport)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	if !errors.As(err, &boundary) || boundary.Fault() != expected ||
		!errors.Is(err, contentflow.ErrFragmentInactivity) {
		t.Fatalf("fragment inactivity boundary=%v", err)
	}
}

func TestReceiverBlockLaneRetiresAnInactiveOperationBeforeReassignment(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	startReceiverPeerWriter(t, runtime)

	share := id16[catalog.ShareInstance](60)
	geometry, err := content.NewFileGeometry(uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share,
		id16[catalog.FileID](61),
		id16[content.FileRevision](62),
		geometry,
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lease := id16[content.LeaseID](63)
	revisions := &receiverRevisionClient{leases: map[content.LeaseID]*remoteLeaseState{
		lease: {id: lease},
	}}
	reassemblyLimits := contentflow.ReassemblyLimits{Bytes: 1 << 20, Records: 8}
	processReassembly, _ := contentflow.NewReassemblyAccount("inactive-process", reassemblyLimits)
	shareReassembly, _ := contentflow.NewReassemblyAccount("inactive-share", reassemblyLimits)
	sessionReassembly, _ := contentflow.NewReassemblyAccount("inactive-session", reassemblyLimits)
	assembler, err := contentflow.NewAssembler(runtime.ProtocolSessionID(), contentflow.ReassemblyHierarchy{
		Process: processReassembly, Share: shareReassembly, Session: sessionReassembly,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const testInactivityTimeout = 20 * time.Millisecond
	lane := &receiverBlockLane{
		identity: runtimeIdentity(runtime), rpc: newTestRequests(runtime, &deterministicReader{next: 64}),
		assembler: assembler, revisions: revisions, fragmentInactivityTimeout: testInactivityTimeout,
	}

	_, fetchErr := lane.FetchBlock(t.Context(), transferlanes.Demand{
		LeaseID: lease, Descriptor: descriptor, Index: 0,
	}, nil)
	capabilityType := reflect.TypeOf(transferlanes.NewDemandReassignableAfterRetirement(nil))
	var boundary *transferfault.BoundaryError
	expected, buildErr := transferfault.NewSession(transferfault.ScopeOutputPause, transferfault.SessionTransport)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	if reflect.TypeOf(fetchErr) != capabilityType ||
		!errors.Is(fetchErr, contentflow.ErrBlockResponseInactivity) ||
		!errors.As(fetchErr, &boundary) || boundary.Fault() != expected {
		t.Fatalf("inactive block result=%T %v", fetchErr, fetchErr)
	}
	assertRequestsRetired(t, lane.rpc)
}

func TestReceiverBlockLaneMarksOnlyPreAdmissionFailureReassignable(t *testing.T) {
	fixture := newBlockProtocolFixture(t, false)
	lane, demand := fixture.lane, fixture.demand
	lane.identity = session.LaneIdentity{ID: runtimeIdentity(fixture.runtime).ID + 1, Epoch: 1}
	_, err := lane.FetchBlock(context.Background(), demand, nil)
	capabilityType := reflect.TypeOf(transferlanes.NewDemandNotAdmitted(sessioncore.ErrLaneUnavailable))
	if reflect.TypeOf(err) != capabilityType || !errors.Is(err, sessioncore.ErrLaneUnavailable) {
		t.Fatalf("pre-admission error=%v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = lane.FetchBlock(cancelled, demand, nil)
	if !errors.Is(err, context.Canceled) || reflect.TypeOf(err) == capabilityType {
		t.Fatalf("cancelled fetch error=%v", err)
	}
}

type uncertainReceiverRequestChannel struct {
	*memoryChannel
	sends      atomic.Int32
	settleSend <-chan struct{}
}

func (channel *uncertainReceiverRequestChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.sends.Add(1)
	if err := channel.memoryChannel.Send(ctx, frame); err != nil {
		return err
	}
	<-channel.settleSend
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
	transferlanes.Demand, func(uint64),
) (records.BlockRecord, error) {
	lane.calls.Add(1)
	return records.BlockRecord{}, errors.New("fallback was reached")
}

type signalingReceiverBlockLane struct {
	inner transferlanes.BlockSource
	done  chan struct{}
	once  sync.Once
}

func (lane *signalingReceiverBlockLane) FetchBlock(
	ctx context.Context,
	demand transferlanes.Demand, _ func(uint64),
) (records.BlockRecord, error) {
	record, err := lane.inner.FetchBlock(ctx, demand, nil)
	lane.once.Do(func() { close(lane.done) })
	return record, err
}

func TestReceiverBlockLaneUnknownSendDoesNotRetryAfterCallerDeadline(t *testing.T) {
	synctest.Test(t, testReceiverBlockLaneUnknownSendDoesNotRetryAfterCallerDeadline)
}

func testReceiverBlockLaneUnknownSendDoesNotRetryAfterCallerDeadline(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	base, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	settleSend := make(chan struct{})
	releaseSend := sync.OnceFunc(func() { close(settleSend) })
	channel := &uncertainReceiverRequestChannel{memoryChannel: base, settleSend: settleSend}
	identity := session.LaneIdentity{ID: runtimeIdentity(runtime).ID + 1, Epoch: 1}
	if err := runtime.InstallLane(identity, channel, permissiveInboundAuthenticator(), nil, nil); err != nil {
		t.Fatal(err)
	}
	runtime.DetachLane(runtimeIdentity(runtime))
	startReceiverPeerWriter(t, runtime)
	laneDone, _ := runtime.LaneDone(identity)

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
	assembler, err := contentflow.NewAssembler(runtime.ProtocolSessionID(), contentflow.ReassemblyHierarchy{
		Process: processReassembly, Share: shareReassembly, Session: sessionReassembly,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blockLane := &receiverBlockLane{
		identity: identity, rpc: newTestRequests(runtime, &deterministicReader{next: 76}),
		assembler: assembler, revisions: revisions,
	}
	signalingLane := &signalingReceiverBlockLane{inner: blockLane, done: make(chan struct{})}
	contentLanes, err := transferlanes.New(transferlanes.Config{ProtocolSessionID: runtime.ProtocolSessionID()})
	if err != nil {
		t.Fatal(err)
	}
	defer contentLanes.Close()
	if err := contentLanes.Add(transferlanes.Identity{ID: 1, Epoch: 1}, transferlanes.RouteRelay, signalingLane); err != nil {
		t.Fatal(err)
	}
	fallback := &countingReceiverFallbackLane{}
	if err := contentLanes.Add(transferlanes.Identity{ID: 2, Epoch: 1}, transferlanes.RouteRelay, fallback); err != nil {
		t.Fatal(err)
	}
	processBudget, err := transfer.NewPlaintextBudget(uint64(catalog.MinChunkSize) * 3)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
		ShareInstance: share, Blocks: contentLanes, MaxBytes: uint64(catalog.MinChunkSize) * 3,
		ProcessBudget: processBudget, MaxConcurrentBlocks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	defer releaseSend()
	// This boundary checks reconciliation after the caller leaves. A longer
	// live demand may explicitly admit one bounded speculative content rescue.
	const callerDeadline = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := broker.GetBlock(ctx, lease, descriptor, 0)
		result <- err
	}()
	synctest.Wait()
	if channel.sends.Load() != 1 || fallback.calls.Load() != 0 {
		t.Fatalf("pending request sends=%d fallback calls=%d", channel.sends.Load(), fallback.calls.Load())
	}
	time.Sleep(callerDeadline - time.Nanosecond)
	synctest.Wait()
	if len(result) != 0 {
		t.Fatalf("unknown send stopped waiting before the caller deadline: %v (sends=%d fallback=%d)", <-result, channel.sends.Load(), fallback.calls.Load())
	}
	time.Sleep(time.Nanosecond)
	synctest.Wait()
	fetchErr := <-result
	if !errors.Is(fetchErr, context.DeadlineExceeded) {
		t.Fatalf("caller deadline error=%v", fetchErr)
	}
	if reflect.TypeOf(fetchErr) == reflect.TypeOf(transferlanes.NewDemandNotAdmitted(nil)) {
		t.Fatalf("unknown transport result=%v", fetchErr)
	}
	// Keep the ambiguous send pending beyond the rescue cadence after its caller leaves.
	const postDeadlineObservation = time.Second
	time.Sleep(postDeadlineObservation)
	synctest.Wait()
	if channel.sends.Load() != 1 || fallback.calls.Load() != 0 {
		t.Fatalf("pending reconciliation sends=%d fallback calls=%d", channel.sends.Load(), fallback.calls.Load())
	}
	releaseSend()
	synctest.Wait()
	select {
	case <-signalingLane.done:
	case <-time.After(time.Second):
		t.Fatal("receiver block lane did not finish unknown-send reconciliation")
	}
	// Reconciliation must not restart canceled demand on a later timer wakeup.
	time.Sleep(postDeadlineObservation)
	synctest.Wait()
	if channel.sends.Load() != 1 || fallback.calls.Load() != 0 {
		t.Fatalf("request sends=%d fallback calls=%d", channel.sends.Load(), fallback.calls.Load())
	}
	<-laneDone
	trackedRequests(blockLane.rpc).mu.Lock()
	activeCalls := len(trackedRequests(blockLane.rpc).calls)
	trackedRequests(blockLane.rpc).mu.Unlock()
	if activeCalls != 0 {
		t.Fatalf("retained consumer calls=%d", activeCalls)
	}
	assertRequestsRetired(t, blockLane.rpc)
}

func TestReceiverBlockLaneProvenDropReassignsDemand(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	lease := id16[content.LeaseID](77)
	revisions := &receiverRevisionClient{leases: map[content.LeaseID]*remoteLeaseState{
		lease: {id: lease},
	}}
	blockLane := &receiverBlockLane{
		identity: session.LaneIdentity{ID: runtimeIdentity(runtime).ID + 1, Epoch: 1},
		rpc:      newTestRequests(runtime, &deterministicReader{next: 78}), revisions: revisions,
		assembler: newBlockProtocolFixture(t, false).lane.assembler,
	}
	contentLanes, err := transferlanes.New(transferlanes.Config{ProtocolSessionID: runtime.ProtocolSessionID()})
	if err != nil {
		t.Fatal(err)
	}
	defer contentLanes.Close()
	if err := contentLanes.Add(transferlanes.Identity{ID: 1, Epoch: 1}, transferlanes.RouteRelay, blockLane); err != nil {
		t.Fatal(err)
	}
	fallback := &countingReceiverFallbackLane{}
	if err := contentLanes.Add(transferlanes.Identity{ID: 2, Epoch: 1}, transferlanes.RouteRelay, fallback); err != nil {
		t.Fatal(err)
	}

	share := id16[catalog.ShareInstance](79)
	geometry, _ := content.NewFileGeometry(uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	descriptor, _ := content.NewFileRevisionDescriptor(
		share, id16[catalog.FileID](80), id16[content.FileRevision](81), geometry, catalog.ModifiedTime{},
	)
	processBudget, _ := transfer.NewPlaintextBudget(uint64(catalog.MinChunkSize) * 3)
	broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
		ShareInstance: share, Blocks: contentLanes, MaxBytes: uint64(catalog.MinChunkSize) * 3,
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
