//go:build darwin

package osfs

import (
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputdarwin"
)

func TestDarwinAPFSRetainedCandidateListAndReopen(t *testing.T) {
	runRetainedCandidateFacadeProof(t)
}

func openStrongNativeOutputPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputdarwin.Open(path, create)
}
