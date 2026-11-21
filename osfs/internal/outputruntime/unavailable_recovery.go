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
func (authority *Authority) retainedSelectionBlocked(
	selection transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) (blocked bool, resultErr error) {
	err := authority.destination.InspectControl(func(control outputcap.Directory) error {
		cursor := checkpointstore.OperationPageCursor{}
		for {
			page, err := checkpointstore.ReadOperationPage(control, cursor, checkpointstore.MaximumOrdinaryOperationPageSizeV1)
			if err != nil {
				return err
			}
			if authority.retainedPageBlocksSelection(page, selection, selector) {
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

func (authority *Authority) retainedPageBlocksSelection(
	page checkpointstore.OperationPage,
	selection transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) bool {
	if page.Unknown() {
		return true
	}
	for _, record := range page.Records() {
		if selector.Kind() == receivecontract.OperationByID && record.OperationID() != selector.ID() {
			continue
		}
		intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
		if err != nil {
			return true
		}
		if !record.Lifecycle().ParticipatesInActiveLookup() {
			continue
		}
		if sameSelection(intent.SelectionSpec(), selection) {
			return true
		}
		if selector.Kind() == receivecontract.OperationByID {
			authority.traceSelectedOperationMismatch(record, intent)
		}
	}
	return false
}
