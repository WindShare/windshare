package outputruntime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3ListingIsolatesUnsafePrivateTreeFromHealthyIntent(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	unsafeSelection := v3RecoverySelection(t, false, 0)
	unsafeOpen := v3RecoveryOpen(t, authority, root, unsafeSelection)
	unsafeSessionPath := v3RecoverySessionPath(
		root, unsafeSelection, unsafeOpen.Session.SessionID(),
	)
	v3RecoveryCloseSession(t, unsafeOpen.Session)
	healthySelection := v3RecoverySelection(t, true, 1)
	healthyOpen := v3RecoveryOpen(t, authority, root, healthySelection)
	v3RecoveryCloseSession(t, healthyOpen.Session)
	if err := runtimeMakePrivateEnvelopeUnsafe(
		filepath.Join(unsafeSessionPath, resumestate.FilesDirectoryName),
	); err != nil {
		t.Fatal(err)
	}

	pins := &atomic.Int64{}
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapInventoryPlatform(platform, pins), nil
	}
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 2 {
		_ = inventory.Close()
		t.Fatalf("isolated intent summaries = %+v, want unsafe and healthy sessions", summaries)
	}
	if actual := pins.Load(); actual != 2 {
		_ = inventory.Close()
		t.Fatalf("live inventory session pins = %d, want 2", actual)
	}
	var unsafeSummary, healthySummary *ResumeStateSummary
	for index := range summaries {
		summary := &summaries[index]
		switch summary.Reference.ResumeIntent() {
		case unsafeSelection.ResumeIntent():
			unsafeSummary = summary
		case healthySelection.ResumeIntent():
			healthySummary = summary
		}
	}
	if unsafeSummary == nil || unsafeSummary.Reference.Kind() != ResumeStateNeedsAttention ||
		!runtimeListingIsolationHasAttention(*unsafeSummary, "unsafe-private-entry") {
		_ = inventory.Close()
		t.Fatalf("unsafe intent summary = %+v", unsafeSummary)
	}
	if healthySummary == nil || healthySummary.Reference.Kind() != ResumeStateRecoverable ||
		len(healthySummary.Attention) != 0 {
		_ = inventory.Close()
		t.Fatalf("healthy sibling summary = %+v", healthySummary)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if actual := pins.Load(); actual != 0 {
		t.Fatalf("inventory close leaked %d isolated session pins", actual)
	}
}

func runtimeListingIsolationHasAttention(summary ResumeStateSummary, expected string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == expected {
			return true
		}
	}
	return false
}
