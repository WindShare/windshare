package resumeauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

func TestInventoryBindsSingleUseReferencesAndRejectsSerialization(t *testing.T) {
	available := listedState(t, 0x61, ListAvailable, nil)
	attention := derivedAttention(AttentionUnknownChildren, []byte("opaque-list-entry"))
	opaque := listedState(t, 0, ListNeedsAttention, []Attention{attention})
	pinned := &fakePinnedInventory{entries: []ListedState{available, opaque}}
	inventory, err := NewInventory(pinned)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 2 || summaries[0].Status() != ListAvailable ||
		summaries[0].CheckpointRecordCount() != 2 || summaries[0].RecoveryArtifactBytes() != 96 ||
		!summaries[1].NeedsAttention() || summaries[1].Attention()[0] != attention {
		t.Fatalf("summaries = %+v", summaries)
	}
	clonedAttention := summaries[1].Attention()
	clonedAttention[0] = Attention{}
	if summaries[1].Attention()[0] != attention {
		t.Fatal("summary leaked mutable attention storage")
	}

	reference := summaries[0].Reference()
	copyOfReference := reference
	claim, err := consumeReference(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumeReference(context.Background(), copyOfReference); !errors.Is(err, ErrReferenceConsumed) {
		t.Fatalf("copied reference error = %v", err)
	}
	claim.Release()

	if _, err := json.Marshal(summaries[1].Reference()); !errors.Is(err, ErrReferenceNotSerializable) {
		t.Fatalf("JSON serialization error = %v", err)
	}
	if _, err := summaries[1].Reference().MarshalText(); !errors.Is(err, ErrReferenceNotSerializable) {
		t.Fatalf("text serialization error = %v", err)
	}
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(summaries[1].Reference()); !errors.Is(err, ErrReferenceNotSerializable) {
		t.Fatalf("gob serialization error = %v", err)
	}

	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeReference(context.Background(), summaries[1].Reference()); !errors.Is(err, ErrInventoryClosed) {
		t.Fatalf("closed inventory reference error = %v", err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if pinned.closeCountValue() != 1 {
		t.Fatalf("pin close count = %d", pinned.closeCountValue())
	}
}

func TestInventoryCloseWaitsForInFlightClaimAndRejectsNewClaims(t *testing.T) {
	pinned := &fakePinnedInventory{entries: []ListedState{
		listedState(t, 0x62, ListAvailable, nil),
		listedState(t, 0x63, ListAvailable, nil),
	}}
	inventory, err := NewInventory(pinned)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	claim, err := consumeReference(context.Background(), summaries[0].Reference())
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- inventory.Close() }()

	waitForInventoryClosing(t, inventory)
	if _, err := consumeReference(context.Background(), summaries[1].Reference()); !errors.Is(err, ErrInventoryClosed) {
		t.Fatalf("claim during close error = %v", err)
	}
	if pinned.closeCountValue() != 0 {
		t.Fatal("native pins closed while a discard claim was in flight")
	}
	claim.Release()
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if pinned.closeCountValue() != 1 {
		t.Fatalf("pin close count = %d", pinned.closeCountValue())
	}
}

func TestReferenceCancellationPrecedesConsumptionAndAcquireBusyConsumesOnce(t *testing.T) {
	pinned := &fakePinnedInventory{
		entries:    []ListedState{listedState(t, 0x64, ListAvailable, nil)},
		acquireErr: NewRepositoryError(RepositoryBusy, "acquire intent lease", errors.New("contended")),
	}
	inventory, err := NewInventory(pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inventory.Close() }()
	reference := inventory.Summaries()[0].Reference()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := consumeReference(cancelled, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled claim error = %v", err)
	}
	claim, err := consumeReference(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claim.Acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy acquire error = %v", err)
	}
	if _, err := claim.Acquire(context.Background()); !errors.Is(err, ErrReferenceConsumed) {
		t.Fatalf("second lease acquire error = %v", err)
	}
	claim.Release()
	if _, err := consumeReference(context.Background(), reference); !errors.Is(err, ErrReferenceConsumed) {
		t.Fatalf("busy reference reuse error = %v", err)
	}
	if !slices.Equal(pinned.acquiredIndexes(), []int{0}) {
		t.Fatalf("acquired indexes = %v", pinned.acquiredIndexes())
	}
}

func TestInventoryRejectsInvalidOrDuplicateAdapterEntriesAndClosesPins(t *testing.T) {
	first := listedState(t, 0x65, ListAvailable, nil)
	for name, entries := range map[string][]ListedState{
		"invalid":   {{status: ListStatus(99)}},
		"duplicate": {first, first},
	} {
		t.Run(name, func(t *testing.T) {
			pinned := &fakePinnedInventory{entries: entries}
			if _, err := NewInventory(pinned); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("construction error = %v", err)
			}
			if pinned.closeCountValue() != 1 {
				t.Fatalf("pin close count = %d", pinned.closeCountValue())
			}
		})
	}
	if _, err := consumeReference(context.Background(), Reference{}); !errors.Is(err, ErrReferenceConsumed) {
		t.Fatalf("zero reference error = %v", err)
	}
	if err := (*Inventory)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

type fakePinnedInventory struct {
	mu         sync.Mutex
	entries    []ListedState
	acquireErr error
	acquired   []int
	closeCount int
}

func (inventory *fakePinnedInventory) Entries() []ListedState {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	return slices.Clone(inventory.entries)
}

func (inventory *fakePinnedInventory) Acquire(_ context.Context, index int) (LeasedRepository, error) {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	inventory.acquired = append(inventory.acquired, index)
	return nil, inventory.acquireErr
}

func (inventory *fakePinnedInventory) Close() error {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	inventory.closeCount++
	return nil
}

func (inventory *fakePinnedInventory) closeCountValue() int {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	return inventory.closeCount
}

func (inventory *fakePinnedInventory) acquiredIndexes() []int {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	return slices.Clone(inventory.acquired)
}

func listedState(
	t *testing.T,
	intentSeed byte,
	status ListStatus,
	attention []Attention,
) ListedState {
	t.Helper()
	var intent transfer.TransferIntentDigest
	var backend transfer.OutputBackendID
	if intentSeed != 0 {
		var err error
		intent, err = transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{intentSeed}, sha256.Size))
		if err != nil {
			t.Fatal(err)
		}
		backend, err = transfer.NewOutputBackendID("resumeauthority-test")
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := NewListedState(ListedStateSpec{
		Status: status, Intent: intent, Backend: backend,
		CheckpointRecordCount: 2, RecoveryArtifactBytes: 96, Attention: attention,
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func waitForInventoryClosing(t *testing.T, inventory *Inventory) {
	t.Helper()
	for range 100_000 {
		inventory.state.mu.Lock()
		closing := inventory.state.closing
		inventory.state.mu.Unlock()
		if closing {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("inventory close did not enter the closing state")
}

var _ PinnedInventory = (*fakePinnedInventory)(nil)
var _ = checkpointmodel.MaxCheckpointRecordsPerIntent
