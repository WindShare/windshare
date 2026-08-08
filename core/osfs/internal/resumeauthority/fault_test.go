package resumeauthority

import (
	"context"
	"errors"
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestNormalizeFaultsMapsEveryClosedRepositoryCode(t *testing.T) {
	tests := []struct {
		code RepositoryErrorCode
		want transferfault.CheckpointCode
	}{
		{RepositoryBusy, transferfault.CheckpointBusy},
		{RepositoryCorruptRecord, transferfault.CheckpointCorruptRecord},
		{RepositoryUnsafeInstall, transferfault.CheckpointUnsafeInstall},
		{RepositoryOwnershipMismatch, transferfault.CheckpointOwnershipMismatch},
		{RepositoryStateIO, transferfault.CheckpointStateIO},
	}
	for _, test := range tests {
		t.Run(string(test.code), func(t *testing.T) {
			boundary := NewRepositoryError(test.code, "test", errors.New("diagnostic"))
			fault, normalized := NormalizeFaults(context.Background(), boundary)
			code, checkpoint := fault.CheckpointCode()
			if !normalized || !checkpoint || code != test.want ||
				fault.Scope() != transferfault.ScopeOutputPause {
				t.Fatalf("fault = %v, normalized = %t", fault, normalized)
			}
			if (test.code == RepositoryBusy) != errors.Is(boundary, ErrBusy) {
				t.Fatalf("busy identity mismatch for %s", test.code)
			}
		})
	}
}

func TestNormalizeFaultsJoinsNormalizedSiblingsAndFailsClosedForUnknown(t *testing.T) {
	busy := NewRepositoryError(RepositoryBusy, "lease", errors.New("busy"))
	stateIO := NewRepositoryError(RepositoryStateIO, "sync", errors.New("I/O"))
	forward, ok := NormalizeFaults(context.Background(), busy, stateIO)
	if !ok {
		t.Fatal("normalized sibling faults were treated as cancellation")
	}
	reverse, ok := NormalizeFaults(context.Background(), stateIO, busy)
	if !ok || forward != reverse {
		t.Fatalf("join depends on order: %v != %v", forward, reverse)
	}
	code, _ := forward.CheckpointCode()
	if code != transferfault.CheckpointStateIO {
		t.Fatalf("joined code = %s", code)
	}

	unknown, ok := NormalizeFaults(context.Background(), errors.New("foreign collaborator error"))
	unknownCode, session := unknown.SessionCode()
	if !ok || !session || unknownCode != transferfault.SessionDependencyContract ||
		unknown.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("unknown fault = %v, normalized = %t", unknown, ok)
	}
	invalidProjection := NewRepositoryError(RepositoryErrorCode("foreign"), "test", errors.New("diagnostic"))
	invalidFault, ok := NormalizeFaults(context.Background(), invalidProjection)
	invalidCode, session := invalidFault.SessionCode()
	if !ok || !session || invalidCode != transferfault.SessionDependencyContract {
		t.Fatalf("invalid projection fault = %v, normalized = %t", invalidFault, ok)
	}
}

func TestNormalizeFaultsKeepsCancellationOutsideFaultPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if fault, normalized := NormalizeFaults(ctx, errors.New("ignored")); normalized || !fault.IsZero() {
		t.Fatalf("cancelled context became fault %v", fault)
	}
	if fault, normalized := NormalizeFaults(context.Background(), context.DeadlineExceeded); normalized || !fault.IsZero() {
		t.Fatalf("deadline became fault %v", fault)
	}
	if fault, normalized := NormalizeFaults(context.Background(), nil); !normalized || !fault.IsZero() {
		t.Fatalf("nil outcome = %v, normalized = %t", fault, normalized)
	}
}
