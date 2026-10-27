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
		previous.mu.Lock()
		defer previous.mu.Unlock()
		if !previous.closed {
			return transfer.FileStart{}, errors.Join(ErrTransactionClosed, destination.Close())
		}
		if previous.binding.Descriptor() != file.Descriptor() {
			return blockedLiveStart(file.Target(), transfer.ItemBlockRevisionConflict, destination)
		}
		if previous.file == nil {
			return blockedLiveStart(file.Target(), transfer.ItemBlockOwnedObjectUnknown, destination)
		}
		same, compareErr := previous.parent.SameDirectory(parent)
		if compareErr != nil {
			return transfer.FileStart{}, errors.Join(collaboratorError(ctx, compareErr), destination.Close())
		}
		if !same {
			return blockedLiveStart(file.Target(), transfer.ItemBlockOwnershipUnknown, destination)
		}
		binding, err := transfer.BindFileMaterializationTarget(file.Target(), previous.binding.ObjectIdentity())
		if err != nil {
			return transfer.FileStart{}, errors.Join(err, destination.Close())
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
		files.files[key], retained = next, true
		if next.published {
			next.closed = true
			expected, err := NewFinalExpectation(binding.ObjectIdentity(), binding.ExactSize())
			if err != nil {
				return transfer.FileStart{}, err
			}
			final, err := settlePublication(ctx, destination, next.file, expected, false)
			if err != nil {
				return transfer.FileStart{}, collaboratorError(ctx, err)
			}
			if final.Condition() != FinalOwnedAtExpectedSize {
				return blockedLiveStart(file.Target(), transfer.ItemBlockPublicationAmbiguous, destination)
			}
			settled, err := transfer.NewPreviouslyPublishedFileSettlement(binding)
			return settlementStart(settled, err)
		}
		transaction, err := WrapPartialFileTransaction(next)
		if err != nil {
			return transfer.FileStart{}, err
		}
		progress, err := transfer.VerifyProcessRanges(binding, next.generation, next.written)
		if err != nil {
			return transfer.FileStart{}, err
		}
		return transfer.NewFileTransactionStart(transaction, progress)
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
