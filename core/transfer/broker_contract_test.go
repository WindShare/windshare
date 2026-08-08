package transfer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

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
