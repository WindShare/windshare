//go:build windows

package artifactpublish

import (
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

func openNativePlatform(path string, create bool) (outputcap.Platform, error) {
	return outputwindows.Open(path, create)
}

func openPrivateNativePlatform(path string, create bool) (outputcap.Platform, error) {
	return outputwindows.OpenPrivatePublicationRoot(path, create)
}

func prepareDirectoryCommit(staged []stagedArtifact) error {
	for index := range staged {
		provider, ok := staged[index].file.(outputcap.CloseRevalidationIdentityProvider)
		if !ok {
			return unsafeError("capture staged Windows file identity", nil)
		}
		identity, err := provider.CloseRevalidationIdentity()
		if err != nil || identity.IsZero() {
			return unsafeError("capture staged Windows file identity", err)
		}
		staged[index].closedIdentity = identity
	}

	// NTFS refuses to rename an ancestor while this process retains descendant
	// handles, even when those handles share delete access. Capture identities
	// first, then close every descendant as one deliberate commit preparation;
	// the final namespace is reopened and matched immediately after the rename.
	var closeErr error
	for index := range staged {
		closeErr = errors.Join(closeErr, staged[index].file.Close())
		staged[index].file = nil
	}
	if closeErr != nil {
		return unsafeError("close staged Windows files for directory commit", closeErr)
	}
	return nil
}
