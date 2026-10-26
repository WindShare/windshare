//go:build linux

package outputlinux

import (
	"crypto/rand"
	"testing"
)

// Native latency includes the durability barriers exercised before content can
// start; allocation-only benchmarks would hide the cost of a wider probe seam.
func BenchmarkLinuxDestinationProbe(b *testing.B) {
	root, err := linuxOpenExt4OutputRoot(b.TempDir(), &linuxHostOutputSystem)
	if err != nil {
		b.Fatal(err)
	}
	defer root.close()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := root.probeDestinationCapabilitiesWithRandom(rand.Reader); err != nil {
			b.Fatal(err)
		}
	}
}
