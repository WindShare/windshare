//go:build windows

package outputportable

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestRejectedRenameOperationIsDefiniteUnsupportedPublication(t *testing.T) {
	for _, cause := range []error{windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION, windows.ERROR_CALL_NOT_IMPLEMENTED} {
		outcome, err := publicationResult(noReplaceError(cause))
		if outcome != 0 || !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) || !errors.Is(err, cause) {
			t.Fatalf("unsupported rename = %s, %v; want no mutation and unsupported", outcome, err)
		}
	}
	outcome, err := publicationResult(noReplaceError(windows.ERROR_IO_DEVICE))
	if outcome != outputcap.PublishNoReplaceIndeterminate || !errors.Is(err, windows.ERROR_IO_DEVICE) ||
		errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
		t.Fatalf("I/O failure lost uncertainty: %s, %v", outcome, err)
	}
}
