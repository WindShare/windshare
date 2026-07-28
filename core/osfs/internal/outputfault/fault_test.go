package outputfault

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestSentinelTaxonomyPreservesDistinctErrors(t *testing.T) {
	sentinels := []error{
		ErrUnsupportedVolume,
		ErrRootUnsafe,
		ErrIntentUnsafe,
		ErrSessionActive,
		ErrSessionClosed,
		ErrFileActive,
		ErrTransactionLimit,
		ErrInspectionLimit,
		ErrLegacyState,
		ErrReservedPath,
		ErrAncestryAuthorityDenied,
	}
	seen := make(map[error]struct{}, len(sentinels))
	for _, sentinel := range sentinels {
		if sentinel == nil || sentinel.Error() == "" {
			t.Fatal("fault sentinel is empty")
		}
		if _, duplicate := seen[sentinel]; duplicate {
			t.Fatalf("fault sentinel identity is reused: %v", sentinel)
		}
		seen[sentinel] = struct{}{}
	}
}

func TestNewPreservesTypedScopeCodeAndCause(t *testing.T) {
	cause := ErrRootUnsafe
	failure := New(transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe, cause)
	var typed *transfer.OutputFault
	if !errors.As(failure, &typed) ||
		typed.Scope() != transfer.OutputFaultRoot ||
		typed.Code() != transfer.OutputFaultNamespaceUnsafe ||
		!errors.Is(failure, cause) {
		t.Fatalf("typed output fault lost scope, code, or cause: %v", failure)
	}
	if failure := New(0, transfer.OutputFaultNamespaceUnsafe, cause); !errors.Is(failure, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid typed fault was accepted: %v", failure)
	}
}
