//go:build !windows

package checkpointstore

func platformCandidateContention(error) bool { return false }
