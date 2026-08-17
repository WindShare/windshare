//go:build windows

package osfs

import "testing"

func TestWindowsNTFSRetainedCandidateListAndReopen(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	runRetainedCandidateFacadeProof(t)
}
