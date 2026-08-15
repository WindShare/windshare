package outputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func openNativeLiveCleanupJournal(
	control outputcap.Directory,
) (destinationauthority.LiveCleanupJournalHandle, error) {
	journal, err := checkpointstore.OpenLiveCleanupJournal(control)
	if err != nil {
		return destinationauthority.LiveCleanupJournalHandle{}, err
	}
	handle, err := destinationauthority.NewLiveCleanupJournalHandle(&journal)
	if err != nil {
		return destinationauthority.LiveCleanupJournalHandle{}, errors.Join(err, journal.Close())
	}
	return handle, nil
}

func checkpointRuntimeError(ctx context.Context, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return transferfault.Wrap(checkpointRuntimeFault(cause), fmt.Errorf("%s: %w", operation, cause))
}

func checkpointRuntimeFault(cause error) transferfault.Fault {
	value, ok := transferfault.NormalizeBoundaryError(cause).Fault()
	if !ok || value.Domain() != transferfault.DomainCheckpoint ||
		value.Scope() != transferfault.ScopeOutputPause {
		return transferfault.DependencyContractFault()
	}
	return value
}
