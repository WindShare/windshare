package fileexecution

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// LiveFiles owns unfinished native objects across attempts. Completed delivery
// receipts have a separate lifetime and never consume native retention slots.
type LiveFiles struct {
	mu         sync.Mutex
	files      map[transfer.MaterializationLocatorDigest]*livePartialFileStrategy
	pending    map[transfer.MaterializationLocatorDigest]*livePartialFileStrategy
	receipts   DeliveryReceipts
	intent     transfer.ReceiveIntent
	trace      TraceSink
	sequence   uint64
	releaseErr error
	closed     bool
}

const maximumUnsettledLiveFiles = 4096

var ErrLiveRetentionLimit = errors.New("unfinished process-local output retention capacity exhausted")

func NewLiveFiles(receipts DeliveryReceipts, intent transfer.ReceiveIntent, trace TraceSink) (*LiveFiles, error) {
	if receipts == nil || intent.IsZero() {
		return nil, ErrInvalidConfiguration
	}
	return &LiveFiles{receipts: receipts, intent: intent, trace: trace}, nil
}

type liveParentSource interface {
	WithExactParent(context.Context, func(outputcap.Directory) error) error
}

func (files *LiveFiles) Begin(
	ctx context.Context, file transfer.MaterializationFile, destination FileDestination,
	create func() (*PartialFileTransaction, error),
) (transfer.FileStart, error) {
	files.mu.Lock()
	defer files.mu.Unlock()
	if files.closed || files.receipts == nil || ctx == nil || destination == nil || create == nil {
		return transfer.FileStart{}, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, errors.Join(err, destination.Close())
	}
	// A receipt-write failure pauses admission without retracting the delivery
	// that already happened. Its exact native state remains available for retry.
	for _, state := range files.pending {
		if err := files.completeLocked(state); err != nil {
			return transfer.FileStart{}, errors.Join(err, destination.Close())
		}
	}
	key := file.Target().Locator().Digest()
	receipt, delivered, err := files.receipts.Lookup(key)
	if err != nil {
		return transfer.FileStart{}, errors.Join(liveReceiptError(err), destination.Close())
	}
	if delivered {
		if receipt.DescriptorDigest != deliveryDescriptorDigest(file.Descriptor()) {
			return blockedLiveStart(file.Target(), transfer.ItemBlockRevisionConflict, destination)
		}
		binding, err := transfer.BindFileMaterializationTarget(file.Target(), receipt.Object)
		if err != nil {
			return transfer.FileStart{}, errors.Join(err, destination.Close())
		}
		settled, err := transfer.NewPreviouslyPublishedFileSettlement(binding)
		files.emitLocked(binding, TraceDeliveryReceipt, TraceReconciled, err)
		return settlementStart(settled, errors.Join(err, destination.Close()))
	}
	parentSource, ok := destination.(liveParentSource)
	if !ok {
		return transfer.FileStart{}, errors.Join(ErrPortContract, destination.Close())
	}
	var parent outputcap.Directory
	err = parentSource.WithExactParent(ctx, func(borrowed outputcap.Directory) error {
		var err error
		parent, err = borrowed.Duplicate()
		return err
	})
	if err != nil {
		return transfer.FileStart{}, errors.Join(err, destination.Close())
	}
	retained := false
	defer func() {
		if !retained {
			_ = parent.Close()
		}
	}()
	if previous := files.files[key]; previous != nil {
		next, start, err := reopenLiveFile(ctx, file, destination, parent, previous)
		if next != nil {
			files.files[key], retained = next, true
			start, err = files.managedStart(next, start, err)
		}
		return start, err
	}
	final, err := destination.ObserveFinalPresence(ctx)
	if err != nil {
		return transfer.FileStart{}, errors.Join(err, destination.Close())
	}
	if final.Condition() != FinalAbsent {
		settled, err := transfer.NewCollisionFileSettlement(file.Target())
		return settlementStart(settled, errors.Join(err, destination.Close()))
	}
	if len(files.files) >= maximumUnsettledLiveFiles {
		// Waiting here can deadlock an attempt whose slots belong to paused
		// files. Pause the output once; retry can settle those retained objects.
		err := newOutputFault(fault.ScopeOutputPause, fault.OutputStateIO, ErrLiveRetentionLimit)
		files.emitLocked(transfer.MaterializedFileBinding{}, TraceLiveRetention, TraceNeedsAttention, err)
		return transfer.FileStart{}, errors.Join(err, destination.Close())
	}
	transaction, err := create()
	if err != nil {
		return transfer.FileStart{}, err
	}
	state, ok := transaction.strategy.(*livePartialFileStrategy)
	if !ok {
		return transfer.FileStart{}, errors.Join(ErrPortContract, destination.Close())
	}
	state.parent = parent
	if files.files == nil {
		files.files = make(map[transfer.MaterializationLocatorDigest]*livePartialFileStrategy)
	}
	files.files[key], retained = state, true
	progress, err := transfer.VerifyProcessRanges(transaction.Binding(), 0, state.written)
	if err != nil {
		return transfer.FileStart{}, err
	}
	start, err := transfer.NewFileTransactionStart(transaction, progress)
	return files.managedStart(state, start, err)
}

// Rebinding transfers both handles together so a retry cannot combine old data
// evidence with a replacement parent reached through the same path.
func reopenLiveFile(
	ctx context.Context, file transfer.MaterializationFile, destination FileDestination,
	parent outputcap.Directory, previous *livePartialFileStrategy,
) (*livePartialFileStrategy, transfer.FileStart, error) {
	previous.mu.Lock()
	defer previous.mu.Unlock()
	blocked := func(reason transfer.ItemBlockReason) (*livePartialFileStrategy, transfer.FileStart, error) {
		start, err := blockedLiveStart(file.Target(), reason, destination)
		return nil, start, err
	}
	if previous.state == liveFileOpen {
		return nil, transfer.FileStart{}, errors.Join(ErrTransactionClosed, destination.Close())
	}
	if previous.binding.Descriptor() != file.Descriptor() {
		return blocked(transfer.ItemBlockRevisionConflict)
	}
	if previous.state == liveFileDelivered {
		// Publication is terminal even before the owning wrapper records its
		// receipt. Concurrent admission must never turn it back into a writer.
		binding, err := transfer.BindFileMaterializationTarget(file.Target(), previous.binding.ObjectIdentity())
		if err != nil {
			return nil, transfer.FileStart{}, errors.Join(err, destination.Close())
		}
		settled, err := transfer.NewPreviouslyPublishedFileSettlement(binding)
		start, err := settlementStart(settled, errors.Join(err, destination.Close()))
		return nil, start, err
	}
	if previous.file == nil {
		return blocked(transfer.ItemBlockOwnedObjectUnknown)
	}
	same, err := previous.parent.SameDirectory(parent)
	if err != nil {
		return nil, transfer.FileStart{}, errors.Join(collaboratorError(ctx, err), destination.Close())
	}
	if !same {
		return blocked(transfer.ItemBlockOwnershipUnknown)
	}
	binding, err := transfer.BindFileMaterializationTarget(file.Target(), previous.binding.ObjectIdentity())
	if err != nil {
		return nil, transfer.FileStart{}, errors.Join(err, destination.Close())
	}
	next := &livePartialFileStrategy{
		materialization: file, destination: destination, file: previous.file,
		binding: binding, cleanup: previous.cleanup, written: previous.written,
		generation: previous.generation, parent: parent, state: liveFileOpen,
	}
	// Old attempt references remain closed; only this new binding can mutate.
	_ = previous.destination.Close()
	_ = previous.parent.Close()
	previous.file, previous.destination, previous.parent = nil, nil, nil
	transaction, err := WrapPartialFileTransaction(next)
	if err != nil {
		return next, transfer.FileStart{}, err
	}
	progress, err := transfer.VerifyProcessRanges(next.binding, next.generation, next.written)
	if err != nil {
		return next, transfer.FileStart{}, err
	}
	start, err := transfer.NewFileTransactionStart(transaction, progress)
	return next, start, err
}

func blockedLiveStart(target transfer.FileMaterializationTarget, reason transfer.ItemBlockReason, destination FileDestination) (transfer.FileStart, error) {
	ref, err := transfer.NewMaterializationStateRef(target.OutputSessionID(), target.Locator().Digest())
	if err != nil {
		return transfer.FileStart{}, err
	}
	settled, err := transfer.NewImmediateItemBlockedFileSettlement(target, ref, reason)
	return settlementStart(settled, errors.Join(err, destination.Close()))
}

// Close removes only private stages whose handles still prove ownership. Finals
// and unrelated objects remain outside the cleanup authority.
func (files *LiveFiles) Close() error {
	if files == nil {
		return nil
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	if files.closed {
		return nil
	}
	files.closed = true
	err := files.releaseErr
	for _, state := range files.files {
		state.mu.Lock()
		err = errors.Join(err, state.close(true))
		state.mu.Unlock()
	}
	if files.receipts != nil {
		err = errors.Join(err, files.receipts.Close())
	}
	files.files, files.pending = nil, nil
	return err
}
