package osfs

import (
	"path/filepath"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
)

// ErrResumeStateBusy is stable across native providers so callers never need
// to inspect native lock errors.
var ErrResumeStateBusy = outputruntime.ErrNativeResumeBusy

// NewFilesystemResumeStateAuthority opens ordinary-v1 operation pages lazily.
// Each list/discard operation reacquires destination and exact operation
// authority; constructing this value performs no checkpoint enumeration.
func NewFilesystemResumeStateAuthority(
	root FilesystemResumeRoot,
) (ResumeStateAuthority, error) {
	if root.RootPath == "" || !filepath.IsAbs(root.RootPath) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	repository, err := outputruntime.NewNativeResumeRepository(
		filepath.Clean(root.RootPath),
		openNativeOutputPlatform,
	)
	if err != nil {
		return nil, err
	}
	return newResumeStateAuthority(repository)
}
