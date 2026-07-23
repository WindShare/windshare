package transfer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type blockingBlockFetcher struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (fetcher *blockingBlockFetcher) fetch(
	ctx context.Context,
	_ BlockDemand,
	_ func(records.BlockRecord) error,
) (records.BlockRecord, error) {
	close(fetcher.started)
	<-ctx.Done()
	close(fetcher.cancelled)
	<-fetcher.release
	return records.BlockRecord{}, ctx.Err()
}

func TestBlockBrokerConcurrentCloseJoinsLoadAndReleasesBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		fetcher := &blockingBlockFetcher{
			started:   make(chan struct{}),
			cancelled: make(chan struct{}),
			release:   make(chan struct{}),
		}
		process, err := NewPlaintextBudget(uint64(catalog.MinChunkSize) * 2)
		if err != nil {
			t.Fatal(err)
		}
		broker, err := newBlockBroker(BlockBrokerConfig{
			ShareInstance: descriptor.ShareInstance(),
			MaxBytes:      uint64(catalog.MinChunkSize),
			ProcessBudget: process,
		}, fetcher)
		if err != nil {
			t.Fatal(err)
		}
		lease := transferID[content.LeaseID](11)

		getDone := make(chan error, 1)
		go func() {
			_, getErr := broker.GetBlock(context.Background(), lease, descriptor, 0)
			getDone <- getErr
		}()
		<-fetcher.started

		firstCloseDone := make(chan struct{})
		secondCloseDone := make(chan struct{})
		go func() {
			broker.Close()
			close(firstCloseDone)
		}()
		go func() {
			broker.Close()
			close(secondCloseDone)
		}()

		<-fetcher.cancelled
		synctest.Wait()
		select {
		case <-firstCloseDone:
			t.Fatal("broker close returned before its owned loader exited")
		default:
		}
		select {
		case <-secondCloseDone:
			t.Fatal("concurrent broker close returned before the shared join completed")
		default:
		}
		close(fetcher.release)
		<-firstCloseDone
		<-secondCloseDone
		if err := <-getDone; !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("closed broker waiter error = %v", err)
		}
		if broker.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("closed broker retained budget: broker=%d process=%d", broker.UsedBytes(), process.Used())
		}
		broker.mu.Lock()
		inflight := len(broker.inflight)
		broker.mu.Unlock()
		if inflight != 0 {
			t.Fatalf("closed broker retained %d inflight loads", inflight)
		}
	})
}

func TestLaneSetCloseJoinsBlockedHedgeAfterFastWinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		winnerRecord := transferRecord(t, descriptor, 0)
		winnerStarted := make(chan struct{})
		loserStarted := make(chan struct{})
		allowWinner := make(chan struct{})
		loserCancelled := make(chan struct{})
		allowLoserExit := make(chan struct{})

		lanes, err := NewLaneSet(LaneSetConfig{
			ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](91),
			RaceWidth:         2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := lanes.Add(LaneIdentity{ID: 1}, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			close(winnerStarted)
			<-allowWinner
			return winnerRecord, nil
		})); err != nil {
			t.Fatal(err)
		}
		if err := lanes.Add(LaneIdentity{ID: 2}, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
			close(loserStarted)
			<-ctx.Done()
			close(loserCancelled)
			<-allowLoserExit
			return records.BlockRecord{}, ctx.Err()
		})); err != nil {
			t.Fatal(err)
		}
		suspendedIdentity := LaneIdentity{ID: 3}
		if err := lanes.Add(suspendedIdentity, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
			<-ctx.Done()
			return records.BlockRecord{}, ctx.Err()
		})); err != nil {
			t.Fatal(err)
		}
		if _, err := lanes.SuspendContent(suspendedIdentity); err != nil {
			t.Fatal(err)
		}
		lanes.mu.Lock()
		winnerState := lanes.lanes[1]
		loserState := lanes.lanes[2]
		lanes.mu.Unlock()

		process, err := NewPlaintextBudget(uint64(catalog.MinChunkSize) * 2)
		if err != nil {
			t.Fatal(err)
		}
		broker, err := NewBlockBroker(BlockBrokerConfig{
			ShareInstance: descriptor.ShareInstance(),
			Lanes:         lanes,
			MaxBytes:      uint64(catalog.MinChunkSize),
			ProcessBudget: process,
		})
		if err != nil {
			t.Fatal(err)
		}
		lease := transferID[content.LeaseID](12)
		fetchDone := make(chan error, 1)
		go func() {
			_, fetchErr := broker.GetBlock(context.Background(), lease, descriptor, 0)
			fetchDone <- fetchErr
		}()
		<-winnerStarted
		<-loserStarted
		close(allowWinner)
		if err := <-fetchDone; err != nil {
			t.Fatalf("fast winner failed: %v", err)
		}
		<-loserCancelled
		brokerCloseDone := make(chan struct{})
		go func() {
			broker.Close()
			close(brokerCloseDone)
		}()
		select {
		case <-brokerCloseDone:
		case <-time.After(time.Second):
			close(allowLoserExit)
			t.Fatal("broker close tried to join a LaneSet-owned hedge")
		}
		if broker.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("broker close retained winner budget: broker=%d process=%d", broker.UsedBytes(), process.Used())
		}
		lanes.mu.Lock()
		closedSignal := lanes.availabilityChanged
		lanes.mu.Unlock()

		firstCloseDone := make(chan struct{})
		secondCloseDone := make(chan struct{})
		go func() {
			lanes.Close()
			close(firstCloseDone)
		}()
		go func() {
			lanes.Close()
			close(secondCloseDone)
		}()
		<-closedSignal
		synctest.Wait()
		select {
		case <-firstCloseDone:
			t.Fatal("lane close returned while its cancelled hedge was still running")
		default:
		}
		select {
		case <-secondCloseDone:
			t.Fatal("concurrent lane close returned before the shared join completed")
		default:
		}

		close(allowLoserExit)
		<-firstCloseDone
		<-secondCloseDone
		lanes.mu.Lock()
		winnerInflight := winnerState.inflight
		loserInflight := loserState.inflight
		remainingLanes := len(lanes.lanes)
		remainingSuspensions := len(lanes.contentSuspensions)
		lanes.mu.Unlock()
		if winnerInflight != 0 || loserInflight != 0 || remainingLanes != 0 || remainingSuspensions != 0 {
			t.Fatalf(
				"closed lane state survived: winner=%d loser=%d lanes=%d suspensions=%d",
				winnerInflight,
				loserInflight,
				remainingLanes,
				remainingSuspensions,
			)
		}
		if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); !errors.Is(err, ErrLaneClosed) {
			t.Fatalf("closed lane set admitted a demand: %v", err)
		}
	})
}

func TestLaneCallbackCanStopWithoutSelfJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		var lanes *LaneSet
		stopReturned := make(chan struct{})
		lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			lanes.Stop()
			close(stopReturned)
			return records.BlockRecord{}, ErrLaneClosed
		})
		var err error
		lanes, err = NewLaneSet(LaneSetConfig{
			ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](92),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := lanes.Add(LaneIdentity{ID: 1}, lane); err != nil {
			t.Fatal(err)
		}

		fetchDone := make(chan error, 1)
		go func() {
			_, fetchErr := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
			fetchDone <- fetchErr
		}()
		select {
		case <-stopReturned:
		case <-time.After(time.Second):
			t.Fatal("callback Stop did not return")
		}
		lanes.Close()
		if err := <-fetchDone; !errors.Is(err, ErrLaneClosed) {
			t.Fatalf("self-stopped lane set error = %v", err)
		}
	})
}

func TestBrokerLaneCallbackCanStopWithoutSelfJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		var broker *BlockBroker
		stopReturned := make(chan struct{})
		lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			broker.Stop()
			close(stopReturned)
			return records.BlockRecord{}, ErrBrokerClosed
		})
		createdBroker, lanes, _, lease, process := newBrokerFixture(
			t,
			1,
			lane,
			uint64(catalog.MinChunkSize),
			uint64(catalog.MinChunkSize)*2,
		)
		broker = createdBroker

		getDone := make(chan error, 1)
		go func() {
			_, getErr := broker.GetBlock(context.Background(), lease, descriptor, 0)
			getDone <- getErr
		}()
		select {
		case <-stopReturned:
		case <-time.After(time.Second):
			t.Fatal("callback Stop did not return")
		}
		broker.Close()
		if err := <-getDone; !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("self-stopped broker waiter error = %v", err)
		}
		if broker.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("self-stopped broker retained budget: broker=%d process=%d", broker.UsedBytes(), process.Used())
		}
		lanes.Close()
	})
}
