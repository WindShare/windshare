//go:build windows

package outputwindows

import (
	"crypto/rand"
	"testing"
)

// The probe runs before destination binding, so extra native round trips affect
// the user's wait before content starts even though they are outside WriteAt.
func BenchmarkWindowsDestinationProbe(b *testing.B) {
	platform, err := openWindowsOutputPlatform(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		b.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := root.probeDestinationCapabilitiesWithRandom(rand.Reader); err != nil {
			b.Fatal(err)
		}
	}
}
