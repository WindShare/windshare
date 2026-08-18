package commandprojection

import (
	"fmt"

	"github.com/windshare/windshare/core/osfs"
)

// filesystemOutputFailure is sealed so only the trusted osfs adapter can grant
// filesystem diagnostics presentation authority. Error lookalikes from callers
// cannot opt themselves into the closed fault mapping through As or Unwrap.
type filesystemOutputFailure struct {
	diagnostic osfs.FilesystemOutputDiagnostic
}

func (failure *filesystemOutputFailure) Error() string {
	if failure == nil || !failure.diagnostic.Valid() {
		return "filesystem output failed"
	}
	return fmt.Sprintf("filesystem output failed at %s", failure.diagnostic.Stage)
}

// SealFilesystemOutputFailure converts the public, value-only osfs diagnostic
// into the one exact error shape trusted by command presentation.
func SealFilesystemOutputFailure(diagnostic osfs.FilesystemOutputDiagnostic) (error, bool) {
	if !diagnostic.Valid() {
		return nil, false
	}
	return &filesystemOutputFailure{diagnostic: diagnostic}, true
}
