//go:build !linux && !windows

package artifactpublish

import (
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func openNativePlatform(string, bool) (outputcap.Platform, error) {
	return nil, errors.New("artifact publication is unsupported on this platform")
}

func openPrivateNativePlatform(string, bool) (outputcap.Platform, error) {
	return nil, errors.New("private artifact publication is unsupported on this platform")
}

func prepareDirectoryCommit([]stagedArtifact) error { return nil }
