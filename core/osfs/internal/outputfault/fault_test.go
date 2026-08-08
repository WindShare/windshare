package outputfault

import (
	"testing"
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
