//go:build darwin

package osfs

import "testing"

func TestDarwinAPFSRetainedCandidateListAndReopen(t *testing.T) {
	runRetainedCandidateFacadeProof(t)
}
