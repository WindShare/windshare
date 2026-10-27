package fileexecution

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// LiveFiles owns process-local evidence across attempts. Parent and data handles
// are retained together: a matching path or file size cannot authorize reuse.
type LiveFiles struct {
	mu     sync.Mutex
	files  map[transfer.MaterializationLocatorDigest]*livePartialFileStrategy
	closed bool
}

const maximumRetainedLiveFiles = 4096

type liveParentSource interface {
	WithExactParent(context.Context, func(outputcap.Directory) error) error
}

func (files *LiveFiles) Begin(
	ctx context.Context, file transfer.MaterializationFile, destination FileDestination,
	create func() (*PartialFileTransaction, error),
) (transfer.FileStart, error) {
	files.mu.Lock()
	defer files.mu.Unlock()
	if files.closed || ctx == nil || destination == nil || create == nil {
		return transfer.FileStart{}, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}
	parentSource, ok := destination.(liveParentSource)
	if !ok {
		return transfer.FileStart{}, ErrPortContract
	}
	var parent outputcap.Directory
	err := parentSource.WithExactParent(ctx, func(borrowed outputcap.Directory) error {
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
	key := file.Target().Locator().Digest()
	previous := files.files[key]
	if previous != nil {
		next, start, err := reopenLiveFile(ctx, file, destination, parent, previous)
		if next != nil {
			files.files[key], retained = next, true
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
	if len(files.files) >= maximumRetainedLiveFiles {
		return transfer.FileStart{}, errors.Join(ErrLiveRetentionLimit, destination.Close())
	}
	transaction, err := create()
	if err != nil {
		return transfer.FileStart{}, err
	}
	state, ok := transaction.strategy.(*livePartialFileStrategy)
	if !ok {
		return transfer.FileStart{}, ErrPortContract
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
	return transfer.NewFileTransactionStart(transaction, progress)
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
	if !previous.closed {
		return nil, transfer.FileStart{}, errors.Join(ErrTransactionClosed, destination.Close())
	}
	if previous.binding.Descriptor() != file.Descriptor() {
		return blocked(transfer.ItemBlockRevisionConflict)
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
		generation: previous.generation, published: previous.published, parent: parent,
	}
	// Old attempt references remain closed; only this new binding can mutate.
	_ = previous.destination.Close()
	_ = previous.parent.Close()
	previous.file, previous.destination, previous.parent = nil, nil, nil
	start, err := next.retainedStart(ctx)
	return next, start, err
}

func (state *livePartialFileStrategy) retainedStart(ctx context.Context) (transfer.FileStart, error) {
	if state.published {
		state.closed = true
		expected, err := NewFinalExpectation(state.binding.ObjectIdentity(), state.binding.ExactSize())
		if err != nil {
			return transfer.FileStart{}, err
		}
		final, err := settlePublication(ctx, state.destination, state.file, expected, false)
		if err != nil {
			return transfer.FileStart{}, collaboratorError(ctx, err)
		}
		if final.Condition() != FinalOwnedAtExpectedSize {
			return blockedLiveStart(state.materialization.Target(), transfer.ItemBlockPublicationAmbiguous, state.destination)
		}
		settled, err := transfer.NewPreviouslyPublishedFileSettlement(state.binding)
		return settlementStart(settled, err)
	}
	transaction, err := WrapPartialFileTransaction(state)
	if err != nil {
		return transfer.FileStart{}, err
	}
	progress, err := transfer.VerifyProcessRanges(state.binding, state.generation, state.written)
	if err != nil {
		return transfer.FileStart{}, err
	}
	return transfer.NewFileTransactionStart(transaction, progress)
}

var ErrLiveRetentionLimit = errors.New("process-local output retention capacity exhausted")

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
	var err error
	for _, state := range files.files {
		state.mu.Lock()
		err = errors.Join(err, state.close(true))
		state.mu.Unlock()
	}
	files.files = nil
	return err
}
