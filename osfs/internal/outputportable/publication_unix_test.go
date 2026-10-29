//go:build linux || darwin

package outputportable

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestRejectedRenameFlagsAreDefiniteUnsupportedPublication(t *testing.T) {
	for _, cause := range []error{unix.EINVAL, unix.ENOSYS, unix.EOPNOTSUPP, unix.ENOTSUP} {
		outcome, err := publicationResult(noReplaceError(cause))
		if outcome != 0 || !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) || !errors.Is(err, cause) {
			t.Fatalf("rejected flag = %s, %v; want no mutation and unsupported", outcome, err)
		}
	}
	outcome, err := publicationResult(noReplaceError(unix.EIO))
	if outcome != outputcap.PublishNoReplaceIndeterminate || !errors.Is(err, unix.EIO) ||
		errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
		t.Fatalf("I/O failure lost uncertainty: %s, %v", outcome, err)
	}
}
