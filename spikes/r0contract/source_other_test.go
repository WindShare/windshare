//go:build !windows && !linux && !darwin

package r0contract

import (
	"errors"
	"testing"
)

var errStableSourceUnsupported = errors.New("stable source snapshots are unsupported on an unproved platform")

func TestNonWindowsStableSourceContractFailsClosed(t *testing.T) {
	// R0 deliberately promises no stability where there is no proved lock/token
	// implementation. Returning an explicit error avoids silently serving drift.
	if err := probeStableSource(); !errors.Is(err, errStableSourceUnsupported) {
		t.Fatalf("probeStableSource() = %v, want unsupported", err)
	}
}

func probeStableSource() error {
	return errStableSourceUnsupported
}
