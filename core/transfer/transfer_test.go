package transfer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func transferID[T ~[16]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func transferDescriptor(t *testing.T, blocks uint64) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(blocks*uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		transferID[catalog.ShareInstance](1), transferID[catalog.FileID](2), transferID[content.FileRevision](3),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func transferRecord(t *testing.T, descriptor content.FileRevisionDescriptor, index uint64) records.BlockRecord {
	t.Helper()
	length, err := descriptor.Geometry().BlockPlainLength(index)
	if err != nil {
		t.Fatal(err)
	}
	record, err := records.NewBlockRecord(descriptor, index, bytes.Repeat([]byte{byte(index)}, int(length)))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type laneFunction func(context.Context, BlockDemand) (records.BlockRecord, error)

func (function laneFunction) FetchBlock(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
	return function(ctx, demand)
}

func validDemand(t *testing.T, descriptor content.FileRevisionDescriptor, index uint64) BlockDemand {
	t.Helper()
	return BlockDemand{LeaseID: transferID[content.LeaseID](4), Descriptor: descriptor, Index: index}
}

func validateTransferRecord(demand BlockDemand) func(records.BlockRecord) error {
	return func(record records.BlockRecord) error {
		if record.Descriptor() != demand.Descriptor || record.LocalBlockIndex() != demand.Index {
			return ErrBlockIdentity
		}
		return nil
	}
}

func TestLaneSetRacesOneWinnerAndCancelsLateLane(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](5), RaceWidth: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	slowStarted := make(chan struct{})
	slowCancelled := make(chan struct{})
	if err := lanes.Add(LaneIdentity{ID: 1}, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
		close(slowStarted)
		<-ctx.Done()
		close(slowCancelled)
		return records.BlockRecord{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 2}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		<-slowStarted
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	record, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
	if err != nil || record.LocalBlockIndex() != 0 {
		t.Fatalf("winner=%+v err=%v", record, err)
	}
	select {
	case <-slowCancelled:
	case <-time.After(time.Second):
		t.Fatal("late racing lane was not cancelled")
	}
}

func TestLaneSetFairnessFailureHotSwitchAndEpochReplacement(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](6), RaceWidth: 1})
	defer lanes.Close()
	var firstCalls, secondCalls atomic.Int32
	first := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		firstCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})
	second := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		secondCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})
	_ = lanes.Add(LaneIdentity{ID: 1}, first)
	_ = lanes.Add(LaneIdentity{ID: 2}, second)
	for range 4 {
		if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
			t.Fatal(err)
		}
	}
	if firstCalls.Load() != 2 || secondCalls.Load() != 2 {
		t.Fatalf("unfair calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}

	failing, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](7), RaceWidth: 1})
	defer failing.Close()
	_ = failing.Add(LaneIdentity{ID: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, errors.New("lane down")
	}))
	_ = failing.Add(LaneIdentity{ID: 2}, second)
	if _, err := failing.fetch(context.Background(), demand, validateTransferRecord(demand)); err == nil {
		t.Fatal("an untyped block failure was unsafely reassigned as a new operation")
	}
	if _, err := failing.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatalf("healthy lane was not selected after failure: %v", err)
	}
	if err := failing.Add(LaneIdentity{ID: 2, Epoch: 0}, second); !errors.Is(err, ErrStaleLane) {
		t.Fatalf("stale epoch error=%v", err)
	}
	replacementCalls := atomic.Int32{}
	replacement := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		replacementCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})
	if err := failing.Add(LaneIdentity{ID: 2, Epoch: 1}, replacement); err != nil {
		t.Fatal(err)
	}
	if failing.Remove(LaneIdentity{ID: 2, Epoch: 0}) || !failing.Remove(LaneIdentity{ID: 2, Epoch: 1}) {
		t.Fatal("lane removal ignored epoch identity")
	}
	if replacementCalls.Load() != 0 {
		t.Fatal("replacement unexpectedly ran")
	}
}

func TestLaneSetRejectsHostileWinnerAndBoundsDynamicLanes(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](8), RaceWidth: 2})
	defer lanes.Close()
	_ = lanes.Add(LaneIdentity{ID: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 1), nil
	}))
	_ = lanes.Add(LaneIdentity{ID: 2}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		time.Sleep(time.Millisecond)
		return transferRecord(t, descriptor, 0), nil
	}))
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatalf("hostile early result defeated valid lane: %v", err)
	}

	full, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](9)})
	defer full.Close()
	for id := uint32(1); id <= MaxLogicalLanes; id++ {
		if err := full.Add(LaneIdentity{ID: id}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	if err := full.Add(LaneIdentity{ID: MaxLogicalLanes + 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrLaneBudget) {
		t.Fatalf("lane budget error=%v", err)
	}
	if full.Len() != MaxLogicalLanes {
		t.Fatalf("lane count=%d", full.Len())
	}
	full.Close()
	if err := full.Add(LaneIdentity{ID: 99}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrLaneClosed) {
		t.Fatalf("closed add error=%v", err)
	}
}

func TestLaneSetSuspendsRelayContentUntilAnotherLaneArrives(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](34),
		RaceWidth:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()

	relayIdentity := LaneIdentity{ID: 1, Epoch: 1}
	var relayCalls atomic.Int32
	if err := lanes.Add(relayIdentity, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		relayCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.SuspendContent(relayIdentity); err != nil {
		t.Fatal(err)
	}

	type fetchResult struct {
		record records.BlockRecord
		err    error
	}
	result := make(chan fetchResult, 1)
	go func() {
		record, fetchErr := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
		result <- fetchResult{record: record, err: fetchErr}
	}()

	var peerCalls atomic.Int32
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		peerCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	select {
	case fetched := <-result:
		if fetched.err != nil || fetched.record.LocalBlockIndex() != 0 {
			t.Fatalf("fetch = %+v, %v", fetched.record, fetched.err)
		}
	case <-time.After(time.Second):
		t.Fatal("new lane did not wake the blocked fetch")
	}
	if relayCalls.Load() != 0 || peerCalls.Load() != 1 {
		t.Fatalf("content calls relay=%d peer=%d", relayCalls.Load(), peerCalls.Load())
	}
}

func TestLaneSetReassignsCurrentDemandWhenSuspendedRelayResumes(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](39),
		RaceWidth:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()

	relayIdentity := LaneIdentity{ID: 1, Epoch: 0}
	var relayCalls atomic.Int32
	if err := lanes.Add(relayIdentity, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		relayCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	relaySuspension, err := lanes.SuspendContent(relayIdentity)
	if err != nil {
		t.Fatal(err)
	}
	peerFailed := make(chan struct{})
	var peerCalls atomic.Int32
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		peerCalls.Add(1)
		close(peerFailed)
		return records.BlockRecord{}, NewDemandNotAdmitted(errors.New("peer path detached before request delivery"))
	})); err != nil {
		t.Fatal(err)
	}

	type fetchResult struct {
		record records.BlockRecord
		err    error
	}
	result := make(chan fetchResult, 1)
	go func() {
		record, fetchErr := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
		result <- fetchResult{record: record, err: fetchErr}
	}()
	<-peerFailed
	if err := relaySuspension.Resume(); err != nil {
		t.Fatal(err)
	}
	select {
	case fetched := <-result:
		if fetched.err != nil || fetched.record.LocalBlockIndex() != demand.Index {
			t.Fatalf("reassigned fetch=%+v err=%v", fetched.record, fetched.err)
		}
	case <-time.After(time.Second):
		t.Fatal("current demand did not resume on the admitted relay lane")
	}
	if peerCalls.Load() != 1 || relayCalls.Load() != 1 {
		t.Fatalf("demand attempts peer=%d relay=%d", peerCalls.Load(), relayCalls.Load())
	}
}

type impostorDemandNotAdmittedError struct{}

func (impostorDemandNotAdmittedError) Error() string      { return "forged pre-admission failure" }
func (impostorDemandNotAdmittedError) DemandNotAdmitted() {}

func TestLaneSetRejectsForgedDemandNotAdmittedMarker(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](45),
		RaceWidth:         1,
	})
	defer lanes.Close()
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, impostorDemandNotAdmittedError{}
	})); err != nil {
		t.Fatal(err)
	}
	var fallbackCalls atomic.Int32
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		fallbackCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err == nil {
		t.Fatal("forged marker authorized a retry")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("forged marker reached fallback %d time(s)", fallbackCalls.Load())
	}
}

func TestLaneSetResumeAndLifecycleWakeBlockedFetches(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](35),
		RaceWidth:         1,
	})
	identity := LaneIdentity{ID: 1, Epoch: 1}
	var calls atomic.Int32
	_ = lanes.Add(identity, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		calls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	}))
	suspension, err := lanes.SuspendContent(identity)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, fetchErr := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
		result <- fetchErr
	}()
	if err := suspension.Resume(); err != nil {
		t.Fatal(err)
	}
	select {
	case fetchErr := <-result:
		if fetchErr != nil {
			t.Fatal(fetchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("resumed lane did not wake the blocked fetch")
	}
	if calls.Load() != 1 {
		t.Fatalf("resumed lane calls=%d", calls.Load())
	}
	lanes.Close()

	empty, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](36)})
	closedResult := make(chan error, 1)
	go func() {
		_, fetchErr := empty.fetch(context.Background(), demand, validateTransferRecord(demand))
		closedResult <- fetchErr
	}()
	empty.Close()
	select {
	case fetchErr := <-closedResult:
		if !errors.Is(fetchErr, ErrLaneClosed) {
			t.Fatalf("closed empty lane set error=%v", fetchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("closing an empty lane set did not wake the blocked fetch")
	}

	cancellable, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](37)})
	defer cancellable.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancelledResult := make(chan error, 1)
	go func() {
		_, fetchErr := cancellable.fetch(ctx, demand, validateTransferRecord(demand))
		cancelledResult <- fetchErr
	}()
	cancel()
	select {
	case fetchErr := <-cancelledResult:
		if !errors.Is(fetchErr, context.Canceled) {
			t.Fatalf("cancelled empty lane set error=%v", fetchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not wake the blocked fetch")
	}
}

func TestLaneSetContentSuspensionUsesExactInitialIdentityAndOpaqueHandle(t *testing.T) {
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](38)})
	identity := LaneIdentity{ID: 1, Epoch: 2}
	if err := lanes.Add(identity, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.SuspendContent(LaneIdentity{}); !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("suspend zero error=%v", err)
	}
	if _, err := lanes.SuspendContent(LaneIdentity{ID: 1, Epoch: 1}); !errors.Is(err, ErrStaleLane) {
		t.Fatalf("suspend stale error=%v", err)
	}
	first, err := lanes.SuspendContent(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.SuspendContent(identity); !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("overlapping suspension error=%v", err)
	}
	if err := first.Resume(); err != nil {
		t.Fatal(err)
	}
	second, err := lanes.SuspendContent(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Resume(); err != nil {
		t.Fatalf("old handle double resume error=%v", err)
	}
	if third, err := lanes.SuspendContent(identity); third != nil || !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("old handle released newer suspension: handle=%v error=%v", third, err)
	}
	lanes.Close()
	if _, err := lanes.SuspendContent(identity); !errors.Is(err, ErrLaneClosed) {
		t.Fatalf("closed suspend error=%v", err)
	}
	if err := second.Resume(); !errors.Is(err, ErrLaneClosed) {
		t.Fatalf("closed resume error=%v", err)
	}
}

func TestLaneSetContentSuspensionFollowsReplacementEpoch(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](42),
		RaceWidth:         1,
	})
	defer lanes.Close()

	initial := LaneIdentity{ID: 1, Epoch: 1}
	if err := lanes.Add(initial, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, errors.New("replaced relay must not be called")
	})); err != nil {
		t.Fatal(err)
	}
	suspension, err := lanes.SuspendContent(initial)
	if err != nil {
		t.Fatal(err)
	}
	if !lanes.Remove(initial) {
		t.Fatal("initial relay was not removed")
	}
	var replacementCalls atomic.Int32
	replacement := LaneIdentity{ID: initial.ID, Epoch: initial.Epoch + 1}
	if err := lanes.Add(replacement, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		replacementCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	var peerCalls atomic.Int32
	peer := LaneIdentity{ID: 2, Epoch: 1}
	if err := lanes.Add(peer, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		peerCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatal(err)
	}
	if replacementCalls.Load() != 0 || peerCalls.Load() != 1 {
		t.Fatalf("held replacement calls=%d peer calls=%d", replacementCalls.Load(), peerCalls.Load())
	}
	if err := suspension.Resume(); err != nil {
		t.Fatal(err)
	}
	if !lanes.Remove(peer) {
		t.Fatal("peer lane was not removed")
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatal(err)
	}
	if replacementCalls.Load() != 1 {
		t.Fatalf("resumed replacement calls=%d", replacementCalls.Load())
	}
}

func TestLaneSetContentSuspensionCanResumeBetweenEpochs(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](43)})
	defer lanes.Close()

	initial := LaneIdentity{ID: 1, Epoch: 3}
	if err := lanes.Add(initial, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, errors.New("removed relay must not be called")
	})); err != nil {
		t.Fatal(err)
	}
	suspension, err := lanes.SuspendContent(initial)
	if err != nil {
		t.Fatal(err)
	}
	if !lanes.Remove(initial) {
		t.Fatal("initial relay was not removed")
	}
	if err := suspension.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := suspension.Resume(); err != nil {
		t.Fatalf("double resume error=%v", err)
	}
	var replacementCalls atomic.Int32
	if err := lanes.Add(LaneIdentity{ID: initial.ID, Epoch: initial.Epoch + 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		replacementCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatal(err)
	}
	if replacementCalls.Load() != 1 {
		t.Fatalf("replacement attached after resume calls=%d", replacementCalls.Load())
	}
}

func TestLaneSetContentSuspensionsShareLogicalLaneBudget(t *testing.T) {
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](44)})
	defer lanes.Close()
	holds := make([]*ContentLaneSuspension, 0, MaxLogicalLanes)
	for index := range MaxLogicalLanes {
		identity := LaneIdentity{ID: uint32(index + 1), Epoch: 1}
		if err := lanes.Add(identity, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatalf("add logical lane %d: %v", index, err)
		}
		hold, err := lanes.SuspendContent(identity)
		if err != nil {
			t.Fatalf("suspend logical lane %d: %v", index, err)
		}
		holds = append(holds, hold)
		if !lanes.Remove(identity) {
			t.Fatalf("remove logical lane %d", index)
		}
	}
	extra := LaneIdentity{ID: MaxLogicalLanes + 1, Epoch: 1}
	if err := lanes.Add(extra, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrLaneBudget) {
		t.Fatalf("held-policy flood add error=%v", err)
	}
	if err := holds[0].Resume(); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(extra, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatalf("released hold did not reopen logical capacity: %v", err)
	}
}

type countingLane struct {
	mu      sync.Mutex
	records map[uint64]records.BlockRecord
	calls   []uint64
	started chan struct{}
	release chan struct{}
}

func (lane *countingLane) FetchBlock(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
	lane.mu.Lock()
	lane.calls = append(lane.calls, demand.Index)
	started, release := lane.started, lane.release
	record := lane.records[demand.Index]
	lane.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return records.BlockRecord{}, ctx.Err()
		case <-release:
		}
	}
	return record, nil
}

func (lane *countingLane) indices() []uint64 {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return slices.Clone(lane.calls)
}

func newBrokerFixture(t *testing.T, blocks uint64, lane BlockLane, maxBytes uint64, processBytes uint64) (*BlockBroker, *LaneSet, content.FileRevisionDescriptor, content.LeaseID, *PlaintextBudget) {
	t.Helper()
	descriptor := transferDescriptor(t, blocks)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](10), RaceWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 1}, lane); err != nil {
		t.Fatal(err)
	}
	process, _ := NewPlaintextBudget(processBytes)
	broker, err := NewBlockBroker(BlockBrokerConfig{
		ShareInstance: descriptor.ShareInstance(), Lanes: lanes, MaxBytes: maxBytes, ProcessBudget: process,
	})
	if err != nil {
		t.Fatal(err)
	}
	return broker, lanes, descriptor, transferID[content.LeaseID](11), process
}

func TestBlockBrokerSingleflightCancellationAndReceiverIsolation(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	lane := &countingLane{
		records: map[uint64]records.BlockRecord{0: transferRecord(t, descriptor, 0)},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	broker, lanes, _, lease, process := newBrokerFixture(t, 1, lane, uint64(catalog.MinChunkSize), uint64(catalog.MinChunkSize)*4)
	defer lanes.Close()
	defer broker.Close()
	firstContext, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := broker.GetBlock(firstContext, lease, descriptor, 0); first <- err }()
	<-lane.started
	second := make(chan []byte, 1)
	go func() { data, _ := broker.GetBlock(context.Background(), lease, descriptor, 0); second <- data }()
	deadline := time.Now().Add(time.Second)
	key := blockKey{file: descriptor.FileID(), revision: descriptor.FileRevision(), index: 0}
	for {
		broker.mu.Lock()
		joined := broker.inflight[key] != nil && broker.inflight[key].waiters == 2
		broker.mu.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second broker consumer did not join")
		}
		time.Sleep(time.Millisecond)
	}
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled consumer error=%v", err)
	}
	close(lane.release)
	if data := <-second; len(data) != catalog.MinChunkSize {
		t.Fatalf("surviving consumer bytes=%d", len(data))
	}
	if data, err := broker.GetBlock(context.Background(), lease, descriptor, 0); err != nil || len(data) != catalog.MinChunkSize || len(lane.indices()) != 1 {
		t.Fatalf("cache hit bytes=%d calls=%v err=%v", len(data), lane.indices(), err)
	}
	if process.Used() != uint64(catalog.MinChunkSize) {
		t.Fatalf("process cache usage=%d", process.Used())
	}

	otherLane := &countingLane{records: map[uint64]records.BlockRecord{0: transferRecord(t, descriptor, 0)}}
	other, otherLanes, _, _, _ := newBrokerFixture(t, 1, otherLane, uint64(catalog.MinChunkSize), uint64(catalog.MinChunkSize)*2)
	defer other.Close()
	defer otherLanes.Close()
	if _, err := other.GetBlock(context.Background(), lease, descriptor, 0); err != nil {
		t.Fatal(err)
	}
	if len(otherLane.indices()) != 1 {
		t.Fatal("receiver broker improperly shared plaintext cache")
	}
}

func TestBlockBrokerRangeReadsOnlyIntersectingLocalBlocks(t *testing.T) {
	descriptor := transferDescriptor(t, 4)
	lane := &countingLane{records: make(map[uint64]records.BlockRecord)}
	for index := range uint64(4) {
		lane.records[index] = transferRecord(t, descriptor, index)
	}
	broker, lanes, _, lease, _ := newBrokerFixture(t, 4, lane, uint64(catalog.MinChunkSize)*3, uint64(catalog.MinChunkSize)*6)
	defer broker.Close()
	defer lanes.Close()
	start := uint64(catalog.MinChunkSize) + 7
	end := uint64(catalog.MinChunkSize)*3 - 11
	var offsets []uint64
	var received []byte
	err := broker.ReadRange(context.Background(), lease, descriptor, content.Range{Offset: start, End: end}, RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
		offsets = append(offsets, offset)
		received = append(received, data...)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	indices := lane.indices()
	slices.Sort(indices)
	if !slices.Equal(indices, []uint64{1, 2}) {
		t.Fatalf("upstream indices=%v", indices)
	}
	if !slices.Equal(offsets, []uint64{start, uint64(catalog.MinChunkSize) * 2}) || uint64(len(received)) != end-start {
		t.Fatalf("offsets=%v bytes=%d", offsets, len(received))
	}
	if !bytes.Equal(received[:catalog.MinChunkSize-7], bytes.Repeat([]byte{1}, catalog.MinChunkSize-7)) {
		t.Fatal("first clipped block bytes changed")
	}
	if err := broker.ReadRange(context.Background(), lease, descriptor, content.Range{}, RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })); !errors.Is(err, ErrInvalidDemand) {
		t.Fatalf("empty range error=%v", err)
	}
	if err := broker.ReadRange(context.Background(), lease, descriptor, content.Range{Offset: 0, End: 1}, nil); err == nil {
		t.Fatal("nil range sink accepted")
	}
}

func TestBlockBrokerReadRangeDispatchesDistinctBlocksAcrossDefaultLanes(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	type dispatch struct {
		lane  uint32
		index uint64
	}
	started := make(chan dispatch, 2)
	release := make(chan struct{})
	newLane := func(laneID uint32) BlockLane {
		return laneFunction(func(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
			started <- dispatch{lane: laneID, index: demand.Index}
			select {
			case <-ctx.Done():
				return records.BlockRecord{}, ctx.Err()
			case <-release:
				return transferRecord(t, descriptor, demand.Index), nil
			}
		})
	}
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](42)})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	if err := lanes.Add(LaneIdentity{ID: 1}, newLane(1)); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 2}, newLane(2)); err != nil {
		t.Fatal(err)
	}
	process, _ := NewPlaintextBudget(uint64(catalog.MinChunkSize) * 2)
	broker, err := NewBlockBroker(BlockBrokerConfig{
		ShareInstance: descriptor.ShareInstance(), Lanes: lanes,
		MaxBytes: uint64(catalog.MinChunkSize) * 2, ProcessBudget: process, MaxConcurrentBlocks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	result := make(chan error, 1)
	go func() {
		result <- broker.ReadRange(
			context.Background(), transferID[content.LeaseID](11), descriptor,
			content.Range{Offset: 0, End: descriptor.ExactSize()},
			RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }),
		)
	}()
	first, second := <-started, <-started
	if first.index == second.index || first.lane == second.lane {
		t.Fatalf("parallel dispatches = %+v and %+v", first, second)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestBlockBrokerRejectsIdentityAndEnforcesSessionProcessBudgets(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	wrong := transferRecord(t, descriptor, 1)
	lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) { return wrong, nil })
	broker, lanes, _, lease, process := newBrokerFixture(t, 2, lane, uint64(catalog.MinChunkSize), uint64(catalog.MinChunkSize))
	defer broker.Close()
	defer lanes.Close()
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 0); !errors.Is(err, ErrBlockIdentity) {
		t.Fatalf("identity error=%v", err)
	}
	if process.Used() != 0 || broker.UsedBytes() != 0 {
		t.Fatalf("failed load leaked budget process=%d broker=%d", process.Used(), broker.UsedBytes())
	}
	if _, err := broker.GetBlock(context.Background(), content.LeaseID{}, descriptor, 0); !errors.Is(err, ErrInvalidDemand) {
		t.Fatalf("zero lease error=%v", err)
	}
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 2); !errors.Is(err, ErrInvalidDemand) {
		t.Fatalf("out of range error=%v", err)
	}

	validLane := &countingLane{records: map[uint64]records.BlockRecord{0: transferRecord(t, descriptor, 0)}}
	tiny, tinyLanes, _, _, _ := newBrokerFixture(t, 2, validLane, uint64(catalog.MinChunkSize)-1, uint64(catalog.MinChunkSize)*2)
	defer tiny.Close()
	defer tinyLanes.Close()
	if _, err := tiny.GetBlock(context.Background(), lease, descriptor, 0); !errors.Is(err, ErrPlaintextBudget) {
		t.Fatalf("session budget error=%v", err)
	}
	broker.Close()
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 0); !errors.Is(err, ErrBrokerClosed) {
		t.Fatalf("closed broker error=%v", err)
	}
}

func TestBlockBrokerInvalidationEvictionAndSharedProcessAdmission(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	lane := &countingLane{records: map[uint64]records.BlockRecord{
		0: transferRecord(t, descriptor, 0), 1: transferRecord(t, descriptor, 1),
	}}
	broker, lanes, _, lease, process := newBrokerFixture(t, 2, lane, uint64(catalog.MinChunkSize), uint64(catalog.MinChunkSize)*2)
	defer broker.Close()
	defer lanes.Close()
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 1); err != nil {
		t.Fatal(err)
	}
	if broker.UsedBytes() != uint64(catalog.MinChunkSize) || process.Used() != uint64(catalog.MinChunkSize) {
		t.Fatalf("eviction usage broker=%d process=%d", broker.UsedBytes(), process.Used())
	}
	if _, err := broker.GetBlock(context.Background(), lease, descriptor, 0); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lane.indices(), []uint64{0, 1, 0}) {
		t.Fatalf("eviction calls=%v", lane.indices())
	}
	broker.InvalidateRevision(descriptor.FileID(), descriptor.FileRevision())
	if broker.UsedBytes() != 0 || process.Used() != 0 {
		t.Fatalf("invalidation usage broker=%d process=%d", broker.UsedBytes(), process.Used())
	}

	sharedProcess, _ := NewPlaintextBudget(uint64(catalog.MinChunkSize))
	setA, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](31), RaceWidth: 1})
	setB, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](32), RaceWidth: 1})
	validLane := laneFunction(func(_ context.Context, demand BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, demand.Index), nil
	})
	_ = setA.Add(LaneIdentity{ID: 1}, validLane)
	_ = setB.Add(LaneIdentity{ID: 1}, validLane)
	brokerA, _ := NewBlockBroker(BlockBrokerConfig{ShareInstance: descriptor.ShareInstance(), Lanes: setA, MaxBytes: uint64(catalog.MinChunkSize), ProcessBudget: sharedProcess})
	brokerB, _ := NewBlockBroker(BlockBrokerConfig{ShareInstance: descriptor.ShareInstance(), Lanes: setB, MaxBytes: uint64(catalog.MinChunkSize), ProcessBudget: sharedProcess})
	defer brokerA.Close()
	defer brokerB.Close()
	defer setA.Close()
	defer setB.Close()
	if _, err := brokerA.GetBlock(context.Background(), lease, descriptor, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := brokerB.GetBlock(context.Background(), lease, descriptor, 1); !errors.Is(err, ErrPlaintextBudget) {
		t.Fatalf("process admission error=%v", err)
	}
}

func TestBlockBrokerInvalidationRejectsLateIgnoringLaneResult(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		close(started)
		<-release
		return transferRecord(t, descriptor, 0), nil
	})
	broker, lanes, _, lease, process := newBrokerFixture(t, 1, lane, uint64(catalog.MinChunkSize), uint64(catalog.MinChunkSize)*2)
	defer broker.Close()
	defer lanes.Close()
	result := make(chan error, 1)
	go func() {
		_, err := broker.GetBlock(context.Background(), lease, descriptor, 0)
		result <- err
	}()
	<-started
	broker.InvalidateRevision(descriptor.FileID(), descriptor.FileRevision())
	select {
	case err := <-result:
		if !errors.Is(err, ErrBlockInvalidated) {
			t.Fatalf("invalidated block error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("invalidation waited for a lane that ignored cancellation")
	}
	if broker.UsedBytes() != 0 || process.Used() != 0 {
		t.Fatalf("invalidated inflight result retained memory broker=%d process=%d", broker.UsedBytes(), process.Used())
	}
	close(release)
}

func TestBlockBrokerCloseUnblocksAnIgnoringLaneAndReleasesAdmission(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		close(started)
		<-release
		return transferRecord(t, descriptor, 0), nil
	})
	broker, lanes, _, lease, process := newBrokerFixture(t, 1, lane, uint64(catalog.MinChunkSize), uint64(catalog.MinChunkSize)*2)
	defer lanes.Close()
	result := make(chan error, 1)
	go func() {
		_, err := broker.GetBlock(context.Background(), lease, descriptor, 0)
		result <- err
	}()
	<-started
	broker.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("closed broker waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("broker close waited for a lane that ignored cancellation")
	}
	if broker.UsedBytes() != 0 || process.Used() != 0 {
		t.Fatalf("closed broker retained inflight memory broker=%d process=%d", broker.UsedBytes(), process.Used())
	}
	close(release)
}

func TestRangeSinkFailureAndLaneLifecycleCancelOnlyCurrentDemand(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	lane := &countingLane{records: map[uint64]records.BlockRecord{
		0: transferRecord(t, descriptor, 0), 1: transferRecord(t, descriptor, 1),
	}}
	broker, lanes, _, lease, _ := newBrokerFixture(t, 2, lane, uint64(catalog.MinChunkSize)*2, uint64(catalog.MinChunkSize)*3)
	defer broker.Close()
	defer lanes.Close()
	sinkErr := errors.New("output stopped")
	if err := broker.ReadRange(context.Background(), lease, descriptor, content.Range{Offset: 1, End: uint64(catalog.MinChunkSize) + 1}, RangeSinkFunc(func(context.Context, uint64, []byte) error {
		return sinkErr
	})); !errors.Is(err, sinkErr) {
		t.Fatalf("sink error=%v", err)
	}
	indices := lane.indices()
	if len(indices) == 0 || len(indices) > 2 || !slices.Contains(indices, uint64(0)) {
		t.Fatalf("sink failure escaped bounded prefetch: %v", indices)
	}

	blockingSet, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](33), RaceWidth: 1})
	started := make(chan struct{})
	_ = blockingSet.Add(LaneIdentity{ID: 1}, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
		close(started)
		<-ctx.Done()
		return records.BlockRecord{}, ctx.Err()
	}))
	result := make(chan error, 1)
	go func() {
		_, err := blockingSet.fetch(context.Background(), validDemand(t, descriptor, 0), validateTransferRecord(validDemand(t, descriptor, 0)))
		result <- err
	}()
	<-started
	blockingSet.Close()
	if err := <-result; !errors.Is(err, ErrLaneClosed) {
		t.Fatalf("closed lane fetch error=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lanes.fetch(cancelled, validDemand(t, descriptor, 0), validateTransferRecord(validDemand(t, descriptor, 0))); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled fetch error=%v", err)
	}
}

func TestConstructorsAndLaneFailuresAreTyped(t *testing.T) {
	if _, err := NewPlaintextBudget(0); err == nil {
		t.Fatal("zero plaintext budget accepted")
	}
	if _, err := NewLaneSet(LaneSetConfig{}); err == nil {
		t.Fatal("zero session lane set accepted")
	}
	if _, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](20), RaceWidth: MaxLogicalLanes + 1}); err == nil {
		t.Fatal("oversized race width accepted")
	}
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](21)})
	defer lanes.Close()
	if err := lanes.Add(LaneIdentity{}, nil); !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("invalid lane error=%v", err)
	}
	if lanes.Remove(LaneIdentity{}) {
		t.Fatal("zero lane identity was removed")
	}
	if _, err := NewBlockBroker(BlockBrokerConfig{}); err == nil {
		t.Fatal("invalid broker accepted")
	}
	var nilBudget *PlaintextBudget
	if nilBudget.Used() != 0 {
		t.Fatal("nil plaintext budget reported usage")
	}
}
