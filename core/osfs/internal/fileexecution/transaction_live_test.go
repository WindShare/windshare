package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestLiveCollisionRetiresJournaledStage(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	native := &liveTransactionFile{data: make([]byte, 4)}
	object, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{0x91}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce:     bytes.Repeat([]byte{0x92}, checkpointmodel.LiveCleanupNonceBytesV1),
		ExactSize: 4, Profile: checkpointmodel.LiveCleanupWindowsNTFSV1,
		Generation: 2, State: checkpointmodel.LiveCleanupStageCreated,
	})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := NewLiveOwnedFile(object, native, ticket)
	if err != nil {
		t.Fatal(err)
	}
	destination := &memoryDestination{target: fixture.file.Target(), publish: FinalCollision}
	cleanupCalls := 0
	transaction, err := NewLivePartialFileTransaction(
		fixture.file, destination, owned,
		func(actual *LiveOwnedFile) error {
			cleanupCalls++
			if actual != owned || actual.CleanupTicket() != ticket {
				t.Fatal("cleanup lost the exact journaled stage authority")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FileCollision {
		t.Fatalf("collision settlement = %d, %v", settlement.Kind(), err)
	}
	if cleanupCalls != 1 || !native.closed || !destination.closed {
		t.Fatalf("collision cleanup = calls %d file %v destination %v", cleanupCalls, native.closed, destination.closed)
	}
}

type liveTransactionFile struct {
	data          []byte
	modified      catalog.ModifiedTime
	syncErr       error
	modifiedErr   error
	metadataErr   error
	closeErr      error
	writeCount    int
	writeCountSet bool
	writeErr      error
	closed        bool
}

func (file *liveTransactionFile) ReadAt(target []byte, offset int64) (int, error) {
	if offset < 0 || offset >= int64(len(file.data)) {
		return 0, io.EOF
	}
	read := copy(target, file.data[offset:])
	if read != len(target) {
		return read, io.EOF
	}
	return read, nil
}

func (file *liveTransactionFile) WriteAt(source []byte, offset int64) (int, error) {
	if offset < 0 || offset+int64(len(source)) > int64(len(file.data)) {
		return 0, io.ErrShortWrite
	}
	if file.writeCountSet {
		count := min(file.writeCount, len(source))
		copy(file.data[offset:], source[:count])
		return count, file.writeErr
	}
	return copy(file.data[offset:], source), file.writeErr
}

func (file *liveTransactionFile) Close() error {
	file.closed = true
	return file.closeErr
}

func (file *liveTransactionFile) Sync() error { return file.syncErr }

func (file *liveTransactionFile) Size() (uint64, error) { return uint64(len(file.data)), nil }

func (file *liveTransactionFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	file.modified = modified
	return file.modifiedErr
}

func (file *liveTransactionFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	return uint64(len(file.data)) == size && file.modified == modified, file.metadataErr
}

func (file *liveTransactionFile) SameFile(other outputcap.File) (bool, error) {
	peer, ok := other.(*liveTransactionFile)
	return ok && peer == file, nil
}

func newLiveTransactionForTest(
	t *testing.T,
	exactSize uint64,
	destination *memoryDestination,
) (*PartialFileTransaction, *liveTransactionFile, *int) {
	t.Helper()
	fixture := newExecutionFixture(t, exactSize)
	native := &liveTransactionFile{data: make([]byte, exactSize)}
	object, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{0xa1}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce:     bytes.Repeat([]byte{0xa2}, checkpointmodel.LiveCleanupNonceBytesV1),
		ExactSize: exactSize, Profile: checkpointmodel.LiveCleanupWindowsNTFSV1,
		Generation: 1, State: checkpointmodel.LiveCleanupStageCreated,
	})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := NewLiveOwnedFile(object, native, ticket)
	if err != nil {
		t.Fatal(err)
	}
	if owned.NativeFile() != native || owned.CleanupTicket() != ticket {
		t.Fatal("live owned file lost its exact cleanup authority")
	}
	destination.target = fixture.file.Target()
	cleanupCalls := new(int)
	transaction, err := NewLivePartialFileTransaction(
		fixture.file, destination, owned,
		func(actual *LiveOwnedFile) error {
			(*cleanupCalls)++
			if actual != owned {
				t.Fatal("cleanup substituted live owned file")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return transaction, native, cleanupCalls
}

func TestLivePartialTransactionCheckpointsOnlyInProcessAndPausesCleanly(t *testing.T) {
	destination := &memoryDestination{final: FinalAbsent}
	transaction, native, cleanupCalls := newLiveTransactionForTest(t, 4, destination)
	if warnings := transaction.MetadataWarnings(); len(warnings) != 0 {
		t.Fatalf("initial metadata warnings = %+v", warnings)
	}
	if err := transaction.WriteRange(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("da")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 1, []byte("x")); !errors.Is(err, ErrRangeOverlap) {
		t.Fatalf("overlapping live write = %v", err)
	}
	if err := transaction.WriteRange(context.Background(), 4, []byte("x")); !errors.Is(err, ErrRangeOutOfBounds) {
		t.Fatalf("out-of-range live write = %v", err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil || checkpoint.CheckpointGeneration() != 1 || len(checkpoint.Ranges().Ranges()) != 0 {
		t.Fatalf("live checkpoint = (generation %d ranges %+v, %v)",
			checkpoint.CheckpointGeneration(), checkpoint.Ranges().Ranges(), err)
	}
	if _, err := transaction.Pause(context.Background(), 0); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid live pause = %v", err)
	}
	settlement, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
	if err != nil || settlement.Kind() != transfer.FilePaused {
		t.Fatalf("live pause = (%d, %v)", settlement.Kind(), err)
	}
	if *cleanupCalls != 1 || !native.closed || !destination.closed {
		t.Fatalf("live pause cleanup = (%d, file %t, destination %t)", *cleanupCalls, native.closed, destination.closed)
	}
	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("closed live checkpoint = %v", err)
	}
}

func TestLivePartialTransactionPublishesWithBestEffortMetadataWarning(t *testing.T) {
	destination := &memoryDestination{final: FinalAbsent}
	transaction, native, cleanupCalls := newLiveTransactionForTest(t, 4, destination)
	native.modifiedErr = errors.New("modified time unavailable")
	if settlement, err := transaction.Commit(context.Background()); !errors.Is(err, ErrIncompleteFile) ||
		settlement.Kind() != 0 {
		t.Fatalf("incomplete live commit = (%d, %v)", settlement.Kind(), err)
	}
	if err := transaction.WriteRange(context.Background(), 2, []byte("ta")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("da")); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("live publish = (%d, %v)", settlement.Kind(), err)
	}
	warnings := transaction.MetadataWarnings()
	if len(warnings) != 1 || warnings[0].Kind() != MetadataModifiedTimeWarning {
		t.Fatalf("live metadata warnings = %+v", warnings)
	}
	if *cleanupCalls != 1 || !native.closed || !destination.closed {
		t.Fatalf("live publish cleanup = (%d, file %t, destination %t)", *cleanupCalls, native.closed, destination.closed)
	}
}

func TestLivePartialTransactionRetireAndCheckpointFailuresStayItemLocal(t *testing.T) {
	destination := &memoryDestination{final: FinalAbsent}
	transaction, native, cleanupCalls := newLiveTransactionForTest(t, 4, destination)
	native.syncErr = errors.New("stage sync failed")
	if _, err := transaction.Checkpoint(context.Background()); err == nil {
		t.Fatal("live checkpoint ignored stage sync failure")
	}
	native.syncErr = nil
	strategy := transaction.strategy.(*livePartialFileStrategy)
	strategy.generation = ^transfer.CheckpointGeneration(0)
	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("live generation overflow = %v", err)
	}
	strategy.generation = 0
	if _, err := transaction.Retire(context.Background(), 0); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid live retire = %v", err)
	}
	settlement, err := transaction.Retire(
		context.Background(), transfer.FileRetireIsolatedPermanentSourceFailure,
	)
	if err != nil || settlement.Kind() != transfer.FileFailed {
		t.Fatalf("live retire = (%d, %v)", settlement.Kind(), err)
	}
	if *cleanupCalls != 1 || !native.closed || !destination.closed {
		t.Fatalf("live retire cleanup = (%d, file %t, destination %t)", *cleanupCalls, native.closed, destination.closed)
	}
}

func TestLivePartialTransactionClassifiesEveryPublicationCut(t *testing.T) {
	t.Run("short-write-without-error", func(t *testing.T) {
		transaction, native, _ := newLiveTransactionForTest(t, 4, &memoryDestination{final: FinalAbsent})
		native.writeCountSet, native.writeCount = true, 1
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write = %v", err)
		}
	})
	t.Run("zero-write-with-error", func(t *testing.T) {
		transaction, native, _ := newLiveTransactionForTest(t, 4, &memoryDestination{final: FinalAbsent})
		failure := errors.New("write rejected")
		native.writeCountSet, native.writeErr = true, failure
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); !errors.Is(err, failure) {
			t.Fatalf("rejected write = %v", err)
		}
	})
	t.Run("stage-sync", func(t *testing.T) {
		transaction, native, _ := newLiveTransactionForTest(t, 4, &memoryDestination{final: FinalAbsent})
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		native.syncErr = errors.New("stage sync failed")
		if settlement, err := transaction.Commit(context.Background()); err == nil || settlement.Kind() != 0 {
			t.Fatalf("stage-sync cut = (%d, %v)", settlement.Kind(), err)
		}
	})

	for name, configure := range map[string]func(*liveTransactionFile, *memoryDestination){
		"publish-ambiguous": func(_ *liveTransactionFile, destination *memoryDestination) {
			destination.publishErr = errors.New("publish result unavailable")
		},
		"foreign-final-state": func(_ *liveTransactionFile, destination *memoryDestination) {
			destination.publish = FinalUnsafe
		},
		"parent-sync": func(_ *liveTransactionFile, destination *memoryDestination) {
			destination.syncErr = errors.New("parent sync failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			destination := &memoryDestination{final: FinalAbsent}
			transaction, native, cleanupCalls := newLiveTransactionForTest(t, 4, destination)
			if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
				t.Fatal(err)
			}
			configure(native, destination)
			settlement, err := transaction.Commit(context.Background())
			reference, reason, blocked := settlement.ItemBlock()
			if err != nil || settlement.Kind() != transfer.FileItemBlocked || !blocked ||
				reference.IsZero() || reason != transfer.ItemBlockPublicationAmbiguous ||
				*cleanupCalls != 0 || !native.closed || !destination.closed {
				t.Fatalf("publication cut = (kind %d blocked %t reason %d cleanup %d file %t destination %t, %v)",
					settlement.Kind(), blocked, reason, *cleanupCalls, native.closed, destination.closed, err)
			}
		})
	}
}

func TestLivePartialTransactionCleanupDebtCannotRetractStableResults(t *testing.T) {
	cleanupFailure := errors.New("cleanup ticket could not be retired")
	installFailure := func(transaction *PartialFileTransaction) {
		strategy := transaction.strategy.(*livePartialFileStrategy)
		original := strategy.cleanup
		strategy.cleanup = func(file *LiveOwnedFile) error {
			_ = original(file)
			return cleanupFailure
		}
	}

	t.Run("published", func(t *testing.T) {
		destination := &memoryDestination{final: FinalAbsent, closeErr: errors.New("destination close failed")}
		transaction, native, cleanupCalls := newLiveTransactionForTest(t, 4, destination)
		native.closeErr = errors.New("stage close failed")
		installFailure(transaction)
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		settlement, err := transaction.Commit(context.Background())
		if err != nil || settlement.Kind() != transfer.FilePublished || *cleanupCalls != 1 {
			t.Fatalf("published cleanup debt = (kind %d cleanup %d, %v)", settlement.Kind(), *cleanupCalls, err)
		}
	})

	t.Run("retired", func(t *testing.T) {
		transaction, _, cleanupCalls := newLiveTransactionForTest(
			t, 4, &memoryDestination{final: FinalAbsent},
		)
		installFailure(transaction)
		settlement, err := transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		)
		_, reason, blocked := settlement.ItemBlock()
		if err != nil || settlement.Kind() != transfer.FileItemBlocked || !blocked ||
			reason != transfer.ItemBlockRetirementUncertain || *cleanupCalls != 1 {
			t.Fatalf("retirement cleanup debt = (kind %d blocked %t reason %d cleanup %d, %v)",
				settlement.Kind(), blocked, reason, *cleanupCalls, err)
		}
	})
}

func TestLivePartialTransactionConstructorAndClosureGuards(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	if transaction, err := NewLivePartialFileTransaction(
		fixture.file, nil, nil, nil,
	); transaction != nil || !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("missing capabilities = (%T, %v)", transaction, err)
	}
	transaction, _, _ := newLiveTransactionForTest(t, 4, destination)
	strategy := transaction.strategy.(*livePartialFileStrategy)
	if err := strategy.validateOpen(nil); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("nil context = %v", err)
	}
	if _, err := transaction.Retire(
		context.Background(), transfer.FileRetireIsolatedPermanentSourceFailure,
	); err != nil {
		t.Fatal(err)
	}
	if err := strategy.close(true); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	if binding := (*livePartialFileStrategy)(nil).Binding(); binding != (transfer.MaterializedFileBinding{}) {
		t.Fatalf("nil live strategy binding = %+v", binding)
	}
}

func TestPartialFileTransactionWrapperNeverManufacturesClosedAuthority(t *testing.T) {
	var transaction *PartialFileTransaction
	if transaction.Binding() != (transfer.MaterializedFileBinding{}) ||
		transaction.MetadataWarnings() != nil {
		t.Fatal("nil partial transaction exposed authority")
	}
	if err := transaction.WriteRange(context.Background(), 0, nil); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("nil write = %v", err)
	}
	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("nil checkpoint = %v", err)
	}
	if _, err := transaction.Commit(context.Background()); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("nil commit = %v", err)
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("nil pause = %v", err)
	}
	if _, err := transaction.Retire(
		context.Background(), transfer.FileRetireIsolatedPermanentSourceFailure,
	); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("nil retire = %v", err)
	}
	if wrapped, err := WrapPartialFileTransaction(nil); wrapped != nil ||
		!errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil strategy wrapper = (%T, %v)", wrapped, err)
	}
}
