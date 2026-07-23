//go:build plan9

package osfs

func isDirectoryNotEmptyError(error) bool {
	// Plan 9 exposes no stable not-empty error identity. Treating a message as
	// authoritative could suppress an unrelated removal failure, so callers
	// must surface the original error instead.
	return false
}
