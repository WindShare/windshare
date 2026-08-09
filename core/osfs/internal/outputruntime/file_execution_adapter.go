package outputruntime

import (
	"context"

	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type fileExecutionAdapter struct{ engine *fileexecution.Engine }

func newFileExecutionAdapter(engine *fileexecution.Engine) fileExecutionAdapter {
	return fileExecutionAdapter{engine: engine}
}

func (adapter fileExecutionAdapter) BeginFile(
	ctx context.Context,
	claim outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	if adapter.engine == nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
			fileexecution.ErrInvalidConfiguration
	}
	start, err := adapter.engine.BeginFile(ctx, claim.File())
	if err != nil {
		return outputsession.FileBeginObservation{Cut: executionMutationCut(err)}, err
	}
	if transaction, durable, active := start.Transaction(); active {
		return outputsession.FileBeginObservation{
			Cut: outputsession.MutationStable, Transaction: fileTransactionAdapter{transaction: transaction},
			Durable: durable,
		}, nil
	}
	settlement, settled := start.ImmediateSettlement()
	if !settled {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous},
			fileexecution.ErrInvalidObservation
	}
	return outputsession.FileBeginObservation{Cut: outputsession.MutationStable, Settlement: settlement}, nil
}

type fileTransactionAdapter struct{ transaction transfer.FileTransaction }

func (adapter fileTransactionAdapter) Binding() transfer.MaterializedFileBinding {
	if adapter.transaction == nil {
		return transfer.MaterializedFileBinding{}
	}
	return adapter.transaction.Binding()
}

func (adapter fileTransactionAdapter) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) (outputsession.MutationCut, error) {
	err := adapter.transaction.WriteRange(ctx, offset, data)
	return successfulOrExecutionCut(err), err
}

func (adapter fileTransactionAdapter) Checkpoint(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, outputsession.MutationCut, error) {
	checkpoint, err := adapter.transaction.Checkpoint(ctx)
	return checkpoint, successfulOrExecutionCut(err), err
}

func (adapter fileTransactionAdapter) Commit(
	ctx context.Context,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	settlement, err := adapter.transaction.Commit(ctx)
	return settlement, successfulOrExecutionCut(err), err
}

func (adapter fileTransactionAdapter) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	settlement, err := adapter.transaction.Pause(ctx, reason)
	return settlement, successfulOrExecutionCut(err), err
}

func (adapter fileTransactionAdapter) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	settlement, err := adapter.transaction.Retire(ctx, reason)
	return settlement, successfulOrExecutionCut(err), err
}

func successfulOrExecutionCut(err error) outputsession.MutationCut {
	if err == nil {
		return outputsession.MutationStable
	}
	return executionMutationCut(err)
}

func executionMutationCut(err error) outputsession.MutationCut {
	result := fault.NormalizeBoundaryError(err)
	value, normalized := result.Fault()
	if normalized {
		if code, output := value.OutputCode(); output && code == fault.OutputMutationAmbiguous {
			return outputsession.MutationAmbiguous
		}
	}
	// fileexecution resolves public publication cuts before returning. Other
	// failures may leave private resumable state, but they do not make the final
	// namespace ownership ambiguous and are safe to reopen through checkpoints.
	return outputsession.MutationNoChange
}
