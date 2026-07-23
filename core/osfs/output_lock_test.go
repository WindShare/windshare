package osfs

import (
	"errors"
	"testing"
)

func TestUnsupportedOutputLockErrorPreservesCapabilityClassification(t *testing.T) {
	err := unsupportedOutputLockError()
	if !errors.Is(err, ErrUnsupportedOutputVolume) {
		t.Fatalf("unsupported lock error lost output capability classification: %v", err)
	}
	if !errors.Is(err, errKernelOutputLockUnavailable) {
		t.Fatalf("unsupported lock error lost exact platform cause: %v", err)
	}
}
