//go:build linux

package osfs

import "testing"

func TestLinuxRetainedCandidateListAndReopen(t *testing.T) {
	runRetainedCandidateFacadeProof(t)
}
