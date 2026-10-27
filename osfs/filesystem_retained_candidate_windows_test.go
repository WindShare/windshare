//go:build windows

package osfs

import "testing"

func TestWindowsNTFSRetainedCandidateListAndReopen(t *testing.T) {
	requireUnprivilegedWindowsNTFSIdentityScheme(t)
	runRetainedCandidateFacadeProof(t)
}
