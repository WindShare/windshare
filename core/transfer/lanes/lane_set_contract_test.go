package lanes

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
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

type laneFunction func(context.Context, Demand) (records.BlockRecord, error)

func (function laneFunction) FetchBlock(ctx context.Context, demand Demand, _ func(uint64)) (records.BlockRecord, error) {
	return function(ctx, demand)
}

func validDemand(t *testing.T, descriptor content.FileRevisionDescriptor, index uint64) Demand {
	t.Helper()
	return Demand{LeaseID: transferID[content.LeaseID](4), Descriptor: descriptor, Index: index}
}

func validateTransferRecord(demand Demand) func(records.BlockRecord) error {
	return func(record records.BlockRecord) error {
		if record.Descriptor() != demand.Descriptor || record.LocalBlockIndex() != demand.Index {
			return fault.Wrap(testProtocolFault(), errTestBlockIdentity)
		}
		return nil
	}
}

func TestLaneSetRacesOneWinnerAndCancelsLateLane(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](5)})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	slowStarted := make(chan struct{})
	slowCancelled := make(chan struct{})
	if err := lanes.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(ctx context.Context, _ Demand) (records.BlockRecord, error) {
		close(slowStarted)
		<-ctx.Done()
		close(slowCancelled)
		return records.BlockRecord{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(Identity{ID: 2}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		<-slowStarted
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	record, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
	if err != nil || record.LocalBlockIndex() != 0 {
		t.Fatalf("winner=%+v err=%v", record, err)
	}
	select {
	case <-slowCancelled:
	case <-time.After(time.Second):
		t.Fatal("late racing lane was not cancelled")
	}
}

func TestLaneSetExplorationFailureHotSwitchAndEpochReplacement(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](6)})
	defer lanes.Close()
	var firstCalls, secondCalls atomic.Int32
	first := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		firstCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})
	second := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		secondCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})
	_ = lanes.Add(Identity{ID: 1}, RouteRelay, first)
	_ = lanes.Add(Identity{ID: 2}, RouteRelay, second)
	for range 4 {
		if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
			t.Fatal(err)
		}
	}
	lanes.attempts.Wait()
	if firstCalls.Load()+secondCalls.Load() != 4 {
		t.Fatalf("bounded exploration calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}

	failing, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](7)})
	defer failing.Close()
	_ = failing.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, errors.New("lane down")
	}))
	_ = failing.Add(Identity{ID: 2}, RouteRelay, second)
	if _, err := failing.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err == nil {
		t.Fatal("an untyped block failure was unsafely reassigned as a new operation")
	}
	if _, err := failing.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
		t.Fatalf("healthy lane was not selected after failure: %v", err)
	}
	if err := failing.Add(Identity{ID: 2, Epoch: 0}, RouteRelay, second); !errors.Is(err, ErrStale) {
		t.Fatalf("stale epoch error=%v", err)
	}
	replacementCalls := atomic.Int32{}
	replacement := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		replacementCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})
	if err := failing.Add(Identity{ID: 2, Epoch: 1}, RouteRelay, replacement); err != nil {
		t.Fatal(err)
	}
	if failing.Remove(Identity{ID: 2, Epoch: 0}) || !failing.Remove(Identity{ID: 2, Epoch: 1}) {
		t.Fatal("lane removal ignored epoch identity")
	}
	if replacementCalls.Load() != 0 {
		t.Fatal("replacement unexpectedly ran")
	}
}

func TestLaneFailureReductionIsPermutationStable(t *testing.T) {
	t.Parallel()

	one := laneResult{
		state:      &laneState{identity: Identity{ID: 1, Epoch: 2}},
		err:        errors.New("lane-one"),
		normalized: failurestate.New(testProtocolFault(), errors.New("terminal")),
	}
	two := laneResult{
		state:      &laneState{identity: Identity{ID: 2, Epoch: 1}},
		err:        errors.New("lane-two"),
		normalized: failurestate.New(testSourceFault(), errors.New("file-local")),
	}
	forward, forwardReassignable := reduceLaneFailures(laneFailureSet{}, []laneResult{one, two})
	reverse, reverseReassignable := reduceLaneFailures(laneFailureSet{}, []laneResult{two, one})
	if forwardReassignable || reverseReassignable {
		t.Fatal("admitted lane failure was marked safe to reassign")
	}
	if forward.failure == nil || reverse.failure == nil ||
		forward.failure.Fault() != reverse.failure.Fault() || forward.failure.Cancellation() != reverse.failure.Cancellation() ||
		forward.failure.Fault() != testProtocolFault() {
		t.Fatalf("permuted fault reductions differ: forward=%+v reverse=%+v", forward.failure, reverse.failure)
	}
	forwardDiagnostic := forward.diagnostic.Error()
	firstLane := strings.Index(forwardDiagnostic, "lane 1/2")
	secondLane := strings.Index(forwardDiagnostic, "lane 2/1")
	if forwardDiagnostic != reverse.diagnostic.Error() ||
		firstLane < 0 || secondLane < 0 || firstLane >= secondLane {
		t.Fatalf("lane diagnostics are not identity-ordered: %q / %q", forwardDiagnostic, reverse.diagnostic.Error())
	}
}

func TestLaneSetRejectsHostileWinnerAndBoundsDynamicLanes(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](8)})
	defer lanes.Close()
	_ = lanes.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 1), nil
	}))
	_ = lanes.Add(Identity{ID: 2}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		time.Sleep(time.Millisecond)
		return transferRecord(t, descriptor, 0), nil
	}))
	if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); !errors.Is(err, errTestBlockIdentity) {
		t.Fatalf("hostile authenticated result lost its protocol meaning: %v", err)
	}

	full, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](9)})
	defer full.Close()
	for id := uint32(1); id <= MaxLogicalLanes; id++ {
		if err := full.Add(Identity{ID: id}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	if err := full.Add(Identity{ID: MaxLogicalLanes + 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrBudget) {
		t.Fatalf("lane budget error=%v", err)
	}
	if full.Len() != MaxLogicalLanes {
		t.Fatalf("lane count=%d", full.Len())
	}
	full.Close()
	if err := full.Add(Identity{ID: 99}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed add error=%v", err)
	}
}

func TestLaneSetSuspendsRelayContentUntilAnotherLaneArrives(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, err := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](34),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()

	relayIdentity := Identity{ID: 1, Epoch: 1}
	var relayCalls atomic.Int32
	if err := lanes.Add(relayIdentity, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
		record, fetchErr := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
		result <- fetchResult{record: record.BlockRecord, err: fetchErr}
	}()

	var peerCalls atomic.Int32
	if err := lanes.Add(Identity{ID: 2, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
	lanes, err := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](39),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()

	relayIdentity := Identity{ID: 1, Epoch: 0}
	var relayCalls atomic.Int32
	if err := lanes.Add(relayIdentity, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
	if err := lanes.Add(Identity{ID: 2, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
		record, fetchErr := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
		result <- fetchResult{record: record.BlockRecord, err: fetchErr}
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
	lanes, _ := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](45),
	})
	defer lanes.Close()
	if err := lanes.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, impostorDemandNotAdmittedError{}
	})); err != nil {
		t.Fatal(err)
	}
	var fallbackCalls atomic.Int32
	if err := lanes.Add(Identity{ID: 2, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		fallbackCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err == nil {
		t.Fatal("forged marker authorized a retry")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("forged marker reached fallback %d time(s)", fallbackCalls.Load())
	}
}

func TestLaneSetResumeAndLifecycleWakeBlockedFetches(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](35),
	})
	identity := Identity{ID: 1, Epoch: 1}
	var calls atomic.Int32
	_ = lanes.Add(identity, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		calls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	}))
	suspension, err := lanes.SuspendContent(identity)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, fetchErr := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
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

	empty, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](36)})
	closedResult := make(chan error, 1)
	go func() {
		_, fetchErr := empty.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
		closedResult <- fetchErr
	}()
	empty.Close()
	select {
	case fetchErr := <-closedResult:
		if !errors.Is(fetchErr, ErrClosed) {
			t.Fatalf("closed empty lane set error=%v", fetchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("closing an empty lane set did not wake the blocked fetch")
	}

	cancellable, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](37)})
	defer cancellable.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancelledResult := make(chan error, 1)
	go func() {
		_, fetchErr := cancellable.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand)})
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
	lanes, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](38)})
	identity := Identity{ID: 1, Epoch: 2}
	if err := lanes.Add(identity, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.SuspendContent(Identity{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("suspend zero error=%v", err)
	}
	if _, err := lanes.SuspendContent(Identity{ID: 1, Epoch: 1}); !errors.Is(err, ErrStale) {
		t.Fatalf("suspend stale error=%v", err)
	}
	first, err := lanes.SuspendContent(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.SuspendContent(identity); !errors.Is(err, ErrInvalid) {
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
	if third, err := lanes.SuspendContent(identity); third != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("old handle released newer suspension: handle=%v error=%v", third, err)
	}
	lanes.Close()
	if _, err := lanes.SuspendContent(identity); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed suspend error=%v", err)
	}
	if err := second.Resume(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed resume error=%v", err)
	}
}

func TestLaneSetContentSuspensionFollowsReplacementEpoch(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](42),
	})
	defer lanes.Close()

	initial := Identity{ID: 1, Epoch: 1}
	if err := lanes.Add(initial, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
	replacement := Identity{ID: initial.ID, Epoch: initial.Epoch + 1}
	if err := lanes.Add(replacement, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		replacementCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	var peerCalls atomic.Int32
	peer := Identity{ID: 2, Epoch: 1}
	if err := lanes.Add(peer, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		peerCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
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
	if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
		t.Fatal(err)
	}
	if replacementCalls.Load() != 1 {
		t.Fatalf("resumed replacement calls=%d", replacementCalls.Load())
	}
}

func TestLaneSetContentSuspensionCanResumeBetweenEpochs(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](43)})
	defer lanes.Close()

	initial := Identity{ID: 1, Epoch: 3}
	if err := lanes.Add(initial, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
	if err := lanes.Add(Identity{ID: initial.ID, Epoch: initial.Epoch + 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		replacementCalls.Add(1)
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
		t.Fatal(err)
	}
	if replacementCalls.Load() != 1 {
		t.Fatalf("replacement attached after resume calls=%d", replacementCalls.Load())
	}
}

func TestLaneSetContentSuspensionsShareLogicalLaneBudget(t *testing.T) {
	lanes, _ := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](44)})
	defer lanes.Close()
	holds := make([]*ContentSuspension, 0, MaxLogicalLanes)
	for index := range MaxLogicalLanes {
		identity := Identity{ID: uint32(index + 1), Epoch: 1}
		if err := lanes.Add(identity, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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
	extra := Identity{ID: MaxLogicalLanes + 1, Epoch: 1}
	if err := lanes.Add(extra, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrBudget) {
		t.Fatalf("held-policy flood add error=%v", err)
	}
	if err := holds[0].Resume(); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(extra, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatalf("released hold did not reopen logical capacity: %v", err)
	}
}
