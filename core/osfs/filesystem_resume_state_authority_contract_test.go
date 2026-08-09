package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestFilesystemResumeRepositoryRejectsMissingRuntimeAuthority(t *testing.T) {
	ctx := context.Background()
	repository := filesystemResumeRepository{}
	if snapshots, err := repository.List(ctx); snapshots != nil || !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil repository list = (%v, %v)", snapshots, err)
	}
	if lease, err := repository.Acquire(ctx, receivecontract.OperationID{}); lease != nil ||
		!errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil repository acquire = (%v, %v)", lease, err)
	}

	var missing *filesystemResumeRepositoryLease
	if _, err := missing.Snapshot(ctx); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil lease snapshot error = %v", err)
	}
	if _, err := missing.ObserveRecovery(ctx); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil lease recovery error = %v", err)
	}
	if _, err := missing.CleanupOwned(ctx); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil lease cleanup error = %v", err)
	}
	if err := missing.InstallReceipt(ctx, nil); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil lease receipt error = %v", err)
	}
	if err := missing.ReplaceLifecycle(ctx, nil, nil); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil lease lifecycle error = %v", err)
	}
	if err := missing.Close(); err != nil {
		t.Fatalf("nil lease close error = %v", err)
	}

	zeroRuntime := &filesystemResumeRepositoryLease{inner: &outputruntime.NativeResumeLease{}}
	if _, err := zeroRuntime.Snapshot(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero runtime snapshot error = %v", err)
	}
	if _, err := zeroRuntime.ObserveRecovery(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero runtime recovery error = %v", err)
	}
	if _, err := zeroRuntime.CleanupOwned(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero runtime cleanup error = %v", err)
	}
	if err := zeroRuntime.InstallReceipt(ctx, nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero runtime receipt error = %v", err)
	}
	if err := zeroRuntime.ReplaceLifecycle(ctx, nil, nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero runtime lifecycle error = %v", err)
	}
	if err := zeroRuntime.Close(); err != nil {
		t.Fatalf("zero runtime close error = %v", err)
	}
}

func TestFilesystemResumeEvidenceProjectionUsesClosedEnums(t *testing.T) {
	evidence := []struct {
		input outputruntime.NativeResumeEvidenceState
		want  ResumeEvidenceState
	}{
		{input: outputruntime.NativeResumeEvidenceAbsent, want: ResumeEvidenceAbsent},
		{input: outputruntime.NativeResumeEvidenceProven, want: ResumeEvidenceProven},
		{input: outputruntime.NativeResumeEvidenceUnknown, want: ResumeEvidenceUnknown},
		{input: outputruntime.NativeResumeEvidenceState(255), want: 0},
	}
	for _, test := range evidence {
		if got := projectNativeResumeEvidenceState(test.input); got != test.want {
			t.Fatalf("evidence projection for %d = %d", test.input, got)
		}
	}

	cleanup := []struct {
		input outputruntime.NativeResumeCleanupState
		want  ResumeCleanupEvidenceState
	}{
		{input: outputruntime.NativeResumeCleanupPending, want: ResumeCleanupPending},
		{input: outputruntime.NativeResumeCleanupComplete, want: ResumeCleanupComplete},
		{input: outputruntime.NativeResumeCleanupUnknown, want: ResumeCleanupUnknown},
		{input: outputruntime.NativeResumeCleanupState(255), want: 0},
	}
	for _, test := range cleanup {
		if got := projectNativeResumeCleanupState(test.input); got != test.want {
			t.Fatalf("cleanup projection for %d = %d", test.input, got)
		}
	}
}
