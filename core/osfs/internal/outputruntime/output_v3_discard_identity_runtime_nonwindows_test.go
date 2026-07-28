//go:build !windows

package outputruntime

func runtimeDiscardAncestorReplacementMustBeBlocked() bool { return false }

func runtimeDiscardIsBlockedAncestorReplacement(error) bool { return false }
