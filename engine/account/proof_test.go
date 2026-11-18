package account

import (
	"strconv"
	"testing"
)

// This optional benchmark calibrates the operator's difficulty without adding
// CPU work to ordinary test runs. Expected seconds are 2^bits / attempts per second.
func BenchmarkProofAttempt(b *testing.B) {
	prefix := []byte("source-bound-challenge-id")
	buffer := make([]byte, len(prefix), len(prefix)+20)
	copy(buffer, prefix)
	var nonce uint64
	for b.Loop() {
		input := strconv.AppendUint(buffer[:len(prefix)], nonce, 10)
		proofMatches(input, 32)
		nonce++
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "attempts/s")
}
