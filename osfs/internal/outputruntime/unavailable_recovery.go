package outputruntime

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/transfer"
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
			if page.Unknown() {
				blocked = true
				return nil
			}
			for _, record := range page.Records() {
				intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
				if err != nil {
					blocked = true
					return nil
				}
				if record.Lifecycle().ParticipatesInActiveLookup() && intent.SelectionSpec().Digest() == selection.Digest() {
					blocked = true
					return nil
				}
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
