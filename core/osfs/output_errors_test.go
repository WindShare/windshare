package osfs

import (
	"errors"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/pathfailure"
)

func TestPublicPathLimitSentinelProjectsDiagnosticIdentity(t *testing.T) {
	failure := pathfailure.Filesystem("open output", "deep/path", syscall.ENAMETOOLONG)
	if ErrPathTooLong != pathfailure.ErrTooLong ||
		!errors.Is(failure, ErrPathTooLong) ||
		!errors.Is(failure, syscall.ENAMETOOLONG) {
		t.Fatalf("public path-limit identity is not preserved: %v", failure)
	}
}
