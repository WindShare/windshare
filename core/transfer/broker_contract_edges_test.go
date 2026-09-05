package transfer

import (
	"container/list"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestBlockBrokerConstructorAppliesOnlyDocumentedDefaults(t *testing.T) {
	share := transferID[catalog.ShareInstance](251)
	budget, err := NewPlaintextBudget(DefaultSessionPlaintextBytes)
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &blockingBlockFetcher{}

	for _, test := range []struct {
		name    string
		config  BlockBrokerConfig
		fetcher blockFetcher
	}{
		{
			name: "share identity",
			config: BlockBrokerConfig{
				ProcessBudget: budget,
			},
			fetcher: fetcher,
		},
		{
			name: "block fetcher",
			config: BlockBrokerConfig{
				ShareInstance: share, ProcessBudget: budget,
			},
		},
		{
			name: "process budget",
			config: BlockBrokerConfig{
				ShareInstance: share,
			},
			fetcher: fetcher,
		},
	} {
		t.Run("missing "+test.name, func(t *testing.T) {
			if _, err := newBlockBroker(test.config, test.fetcher); err == nil {
				t.Fatalf("broker accepted a missing %s", test.name)
			}
		})
	}
	if _, err := newBlockBroker(BlockBrokerConfig{
		ShareInstance: share, ProcessBudget: budget, MaxConcurrentBlocks: MaximumConcurrentBlocks + 1,
	}, fetcher); err == nil {
		t.Fatal("broker accepted an out-of-contract concurrency limit")
	}

	broker, err := newBlockBroker(BlockBrokerConfig{
		ShareInstance: share, ProcessBudget: budget,
	}, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	if broker.maxBytes != DefaultSessionPlaintextBytes || broker.maxConcurrentBlocks != DefaultConcurrentBlocks {
		t.Fatalf("broker defaults = bytes %d, concurrency %d", broker.maxBytes, broker.maxConcurrentBlocks)
	}
}

func TestBlockBrokerReservationCannotEvictAnInflightOwner(t *testing.T) {
	budget, err := NewPlaintextBudget(2)
	if err != nil {
		t.Fatal(err)
	}
	broker := &BlockBroker{maxBytes: 2, process: budget, entries: make(map[blockKey]*list.Element), lru: list.New()}
	if err := broker.reserveLocked(2); err != nil {
		t.Fatalf("initial inflight reservation: %v", err)
	}
	defer broker.releaseCallReservationLocked(&blockCall{reserved: 2})

	if err := broker.reserveLocked(1); !errors.Is(err, ErrPlaintextBudget) {
		t.Fatalf("reservation behind inflight owner error = %v", err)
	}
}

func TestBlockBrokerLastCancelledWaiterOwnsAbandonment(t *testing.T) {
	budget, err := NewPlaintextBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	loadContext, cancelLoad := context.WithCancel(context.Background())
	call := &blockCall{
		ctx: loadContext, cancel: cancelLoad, done: make(chan struct{}), waiters: 1,
	}
	key := blockKey{index: 1}
	broker := &BlockBroker{
		process:  budget,
		inflight: map[blockKey]*blockCall{key: call},
	}
	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if data, err := broker.await(waitContext, key, call); data.data != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = data %v, err %v", data, err)
	}
	if !call.completed || call.waiters != 0 || !errors.Is(call.err, context.Canceled) || broker.inflight[key] != nil {
		t.Fatalf("abandoned call state = %+v, inflight %v", call, broker.inflight[key])
	}
	select {
	case <-call.done:
	default:
		t.Fatal("last waiter did not publish abandoned completion")
	}

	// A loader that observes the already-completed call must not close or mutate
	// it again; this is the callback-side half of the abandonment ownership rule.
	broker.completeAbandonedCallLocked(call, ErrBrokerClosed)
	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("completed abandoned call was overwritten: %v", call.err)
	}
}
