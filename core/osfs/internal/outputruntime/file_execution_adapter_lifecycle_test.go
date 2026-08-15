package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

type retireRecordingTransaction struct {
	reason transfer.FileRetireReason
	err    error
}

func (*retireRecordingTransaction) Binding() transfer.MaterializedFileBinding {
	return transfer.MaterializedFileBinding{}
}

func (*retireRecordingTransaction) WriteRange(context.Context, uint64, []byte) error {
	return nil
}

func (*retireRecordingTransaction) Checkpoint(context.Context) (transfer.VerifiedDurableRanges, error) {
	return transfer.VerifiedDurableRanges{}, nil
}

func (*retireRecordingTransaction) Commit(context.Context) (transfer.FileSettlement, error) {
	return transfer.FileSettlement{}, nil
}

func (*retireRecordingTransaction) Pause(context.Context, transfer.FilePauseReason) (transfer.FileSettlement, error) {
	return transfer.FileSettlement{}, nil
}

func (transaction *retireRecordingTransaction) Retire(
	_ context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, error) {
	transaction.reason = reason
	return transfer.FileSettlement{}, transaction.err
}

func TestFileExecutionAdapterRetiresThroughTheUnifiedTransaction(t *testing.T) {
	failure := errors.New("retirement failed")
	transaction := &retireRecordingTransaction{err: failure}
	settlement, cut, err := (fileTransactionAdapter{transaction: transaction}).Retire(
		context.Background(),
		transfer.FileRetireInvalidatedRevision,
	)
	if !errors.Is(err, failure) || settlement.Kind() != 0 ||
		cut != outputsession.MutationNoChange ||
		transaction.reason != transfer.FileRetireInvalidatedRevision {
		t.Fatalf("retire delegation = (settlement %d, cut %d, reason %d, %v)",
			settlement.Kind(), cut, transaction.reason, err)
	}
}
