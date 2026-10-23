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
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type blockingBlockFetcher struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (fetcher *blockingBlockFetcher) Fetch(
	ctx context.Context,
	request transferlanes.Request) (transferlanes.Block, error) {
	if request.Claim != nil {
		release, _, err := request.Claim()
		if err != nil {
			return transferlanes.Block{}, err
		}
		if release != nil {
			defer release()
		}
	}
	close(fetcher.started)
	<-ctx.Done()
	close(fetcher.cancelled)
	<-fetcher.release
	return transferlanes.Block{}, ctx.Err()
}

func TestBlockBrokerConcurrentCloseJoinsLoadAndReleasesBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		fetcher := &blockingBlockFetcher{
			started:   make(chan struct{}),
			cancelled: make(chan struct{}),
			release:   make(chan struct{}),
		}
		process, err := NewPlaintextBudget(uint64(catalog.MinChunkSize) * 4)
		if err != nil {
			t.Fatal(err)
		}
		broker, err := NewBlockBroker(BlockBrokerConfig{
			Blocks:        fetcher,
			ShareInstance: descriptor.ShareInstance(),
			MaxBytes:      uint64(catalog.MinChunkSize) * 4,
			ProcessBudget: process,
		})
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

func TestBrokerLaneCallbackCanStopWithoutSelfJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		var broker *BlockBroker
		stopReturned := make(chan struct{})
		lane := laneFunction(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
			broker.Stop()
			close(stopReturned)
			return records.BlockRecord{}, ErrBrokerClosed
		})
		createdBroker, lanes, _, lease, process := newBrokerFixture(
			t,
			1,
			lane,
			uint64(catalog.MinChunkSize)*3,
			uint64(catalog.MinChunkSize)*3,
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
		lanes.Close()
		if broker.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("self-stopped broker retained budget: broker=%d process=%d", broker.UsedBytes(), process.Used())
		}
		lanes.Close()
	})
}
