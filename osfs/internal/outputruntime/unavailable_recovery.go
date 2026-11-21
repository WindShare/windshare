package outputruntime

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// A capability downgrade must not turn a retained selection into a fresh miss.
// Other readable, unrelated operations do not prevent safe new live output.
func (authority *Authority) retainedSelectionBlocked(selection transfer.SelectionSpec) (blocked bool, resultErr error) {
	err := authority.destination.InspectControl(func(control outputcap.Directory) error {
		cursor := checkpointstore.OperationPageCursor{}
		for {
			page, err := checkpointstore.ReadOperationPage(control, cursor, checkpointstore.MaximumOrdinaryOperationPageSizeV1)
			if err != nil {
				return err
			}
			if retainedPageBlocksSelection(page, selection) {
				blocked = true
				return nil
			}
			if page.Next().IsZero() {
				return nil
			}
			cursor = page.Next()
		}
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return true, nil
	}
	return blocked, err
}

func retainedPageBlocksSelection(
	page checkpointstore.OperationPage,
	selection transfer.SelectionSpec,
) bool {
	if page.Unknown() {
		return true
	}
	for _, record := range page.Records() {
		if !record.Lifecycle().ParticipatesInActiveLookup() {
			continue
		}
		intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
		if err != nil || sameSelection(intent.SelectionSpec(), selection) {
			return true
		}
	}
	return false
}

// An explicit ID lookup on a downgraded live-only destination inspects only the
// named operation so unrelated corrupt entries neither scan nor block it.
func (authority *Authority) retainedOperationBlocked(
	selection transfer.SelectionSpec,
	operation receivecontract.OperationID,
) (blocked bool, resultErr error) {
	err := authority.destination.InspectControl(func(control outputcap.Directory) error {
		observed, err := checkpointstore.ReadOperation(control, operation)
		if err != nil {
			return err
		}
		switch observed.State() {
		case checkpointstore.ObservedOperationAbsent:
			return nil
		case checkpointstore.ObservedOperationUnknown:
			blocked = true
			return nil
		case checkpointstore.ObservedOperationPresent:
			record := observed.Record()
			if !record.Lifecycle().ParticipatesInActiveLookup() {
				return nil
			}
			intent := observed.Intent()
			if sameSelection(intent.SelectionSpec(), selection) {
				blocked = true
				return nil
			}
			authority.traceSelectedOperationMismatch(record, intent)
			return nil
		default:
			blocked = true
			return nil
		}
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return true, nil
	}
	return blocked, err
}
