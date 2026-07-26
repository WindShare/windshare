//go:build !windows

package osfs

func v3RecoveryAncestorReplacementMustBeBlocked() bool {
	return false
}

func v3RecoveryIsBlockedAncestorReplacement(error) bool {
	return false
}
