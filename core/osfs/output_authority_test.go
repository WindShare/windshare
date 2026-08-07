package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestFilesystemOutputAuthorityRejectsZeroAndNilCapabilities(t *testing.T) {
	var authority *FilesystemOutputAuthority
	if _, err := authority.OpenOutput(context.Background(), transfer.TransferIntent{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority open = %v, want invalid binding", err)
	}

	var trace FilesystemOutputTrace
	var nilTracer FilesystemOutputTraceFunc
	nilTracer.TraceFilesystemOutput(trace)
	called := false
	FilesystemOutputTraceFunc(func(FilesystemOutputTrace) { called = true }).TraceFilesystemOutput(trace)
	if !called {
		t.Fatal("non-nil trace function was not invoked")
	}
}
