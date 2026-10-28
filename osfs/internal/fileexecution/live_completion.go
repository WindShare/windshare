package fileexecution

import (
	"context"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

// The wrapper notifies the operation after releasing the transaction mutex.
// Admission and shutdown always acquire the operation before a transaction.
type retainedLiveTransaction struct {
	*PartialFileTransaction
	files *LiveFiles
	state *livePartialFileStrategy
}

func (files *LiveFiles) managedStart(state *livePartialFileStrategy, start transfer.FileStart, err error) (transfer.FileStart, error) {
	if err != nil {
		return start, err
	}
	transaction, progress, active := start.Transaction()
	if !active {
		return start, nil
	}
	partial, ok := transaction.(*PartialFileTransaction)
	if !ok {
		return transfer.FileStart{}, ErrPortContract
	}
	return transfer.NewFileTransactionStart(&retainedLiveTransaction{
		PartialFileTransaction: partial, files: files, state: state,
	}, progress)
}

func (transaction *retainedLiveTransaction) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	settlement, err := transaction.PartialFileTransaction.Commit(ctx)
	if err == nil && settlement.Kind() == transfer.FilePublished {
		files := transaction.files
		files.mu.Lock()
		defer files.mu.Unlock()
		key := transaction.state.binding.Locator().Digest()
		if !files.closed && files.files[key] == transaction.state {
			if files.pending == nil {
				files.pending = make(map[transfer.MaterializationLocatorDigest]*livePartialFileStrategy)
			}
			files.pending[transaction.state.binding.Locator().Digest()] = transaction.state
			// A bookkeeping or cleanup failure cannot undo successful publication.
			// Admission observes pending receipt errors before starting another file.
			_ = files.completeLocked(transaction.state)
		}
	}
	return settlement, err
}

func (transaction *retainedLiveTransaction) Retire(ctx context.Context, reason transfer.FileRetireReason) (transfer.FileSettlement, error) {
	settlement, err := transaction.PartialFileTransaction.Retire(ctx, reason)
	if err == nil && settlement.Kind() == transfer.FileFailed {
		files := transaction.files
		files.mu.Lock()
		defer files.mu.Unlock()
		key := transaction.state.binding.Locator().Digest()
		if files.files[key] == transaction.state {
			delete(files.files, key)
		}
	}
	return settlement, err
}

func (files *LiveFiles) completeLocked(state *livePartialFileStrategy) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	key := state.binding.Locator().Digest()
	if err := files.receipts.Record(key, deliveredFile(state.binding)); err != nil {
		err = liveReceiptError(err)
		files.emitLocked(state.binding, TraceDeliveryReceipt, TraceNeedsAttention, err)
		return err
	}
	files.emitLocked(state.binding, TraceDeliveryReceipt, TraceSucceeded, nil)
	// Keep exact handles if cleanup is still uncertain. The delivery receipt
	// remains usable, while cleanup debt stays within the unfinished budget.
	if err := state.cleanup(state.file); err != nil {
		files.emitLocked(state.binding, TraceReleaseLiveFile, TraceNeedsAttention, fileStateError(err))
		return nil
	}
	err := state.close(false)
	state.written = content.RangeSet{}
	delete(files.files, key)
	delete(files.pending, key)
	if files.releaseErr == nil {
		files.releaseErr = err
	}
	if err != nil {
		files.emitLocked(state.binding, TraceReleaseLiveFile, TraceNeedsAttention, fileStateError(err))
	} else {
		files.emitLocked(state.binding, TraceReleaseLiveFile, TraceSucceeded, nil)
	}
	return nil
}

func liveReceiptError(cause error) error {
	return newOutputFault(fault.ScopeOutputPause, fault.OutputStateIO, cause)
}

func (files *LiveFiles) emitLocked(binding transfer.MaterializedFileBinding, operation TraceOperation, outcome TraceOutcome, err error) {
	if files.trace == nil {
		return
	}
	files.sequence++
	files.trace.TraceFileExecution(TraceEvent{
		OperationID: files.intent.OperationID(), IntentDigest: files.intent.Digest(),
		SessionID: binding.OutputSessionID(), Sequence: files.sequence, Operation: operation,
		Outcome: outcome, Fault: traceFault(err), RetainedLiveFiles: uint64(len(files.files)),
	})
}
