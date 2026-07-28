package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestFilesystemOutputAuthorityRejectsZeroAndNilCapabilities(t *testing.T) {
	var authority *FilesystemOutputAuthority
	if _, err := authority.OpenSelection(context.Background(), transfer.OutputSelection{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority open = %v, want invalid binding", err)
	}
	var inventory *ResumeStateInventory
	if got := inventory.Summaries(); got != nil {
		t.Fatalf("nil inventory summaries = %#v, want nil", got)
	}
	if err := inventory.Close(); err != nil {
		t.Fatalf("nil inventory close = %v", err)
	}

	var reference ResumeStateRef
	if !reference.ResumeIntent().IsZero() || !reference.SessionID().IsZero() || reference.Kind() != 0 {
		t.Fatalf("zero resume reference leaked authority: intent=%v session=%v kind=%d", reference.ResumeIntent(), reference.SessionID(), reference.Kind())
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

func TestFilesystemOutputAuthorityResumeWrappersClassifyInvalidBindings(t *testing.T) {
	if inventory, err := ListResumeState(context.Background(), FilesystemResumeRoot{}); inventory != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("empty resume root listing = (%v, %v), want invalid binding", inventory, err)
	}
	if settlement, err := DiscardResumeState(context.Background(), ResumeStateRef{}); settlement.Kind != 0 ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero resume discard = (%+v, %v), want invalid binding", settlement, err)
	}
}
