//go:build linux

package artifactpublish

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func openNativePlatform(path string, create bool) (outputcap.Platform, error) {
	return outputlinux.Open(path, create)
}

func openPrivateNativePlatform(path string, create bool) (outputcap.Platform, error) {
	return outputlinux.Open(path, create)
}

func prepareDirectoryCommit([]stagedArtifact) error {
	// Linux renameat2 permits live descendant handles, so the stronger open-file
	// identity remains pinned continuously through the no-replace commit.
	return nil
}
