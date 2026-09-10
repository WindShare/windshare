package directoryauthority

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestCompletedSubtreesReleaseResourcesAndKeepReceipts(t *testing.T) {
	const subtreeCount = 128
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	root := materializeRoot(t, authority, catalog.ModifiedTime{})
	for index := range subtreeCount {
		path := fmt.Sprintf("branch-%d", index)
		branch := mustClaim(t, authority, ClaimID(2+index*2), root.id, path, catalog.ModifiedTime{})
		leaf := mustClaim(t, authority, branch.id+1, branch.id, path+"/leaf", catalog.ModifiedTime{})
		for _, claim := range []directoryClaim{branch, leaf} {
			if _, _, err := materializeNative(authority, t.Context(), claim); err != nil {
				t.Fatal(err)
			}
		}
		if got := platform.openDirectories.Load(); got != 3 {
			t.Fatalf("subtree %d: open directories = %d, want only root/branch/leaf", index, got)
		}
		for _, claim := range []directoryClaim{leaf, branch} {
			result, cached, err := finalizeNative(authority, t.Context(), claim)
			if err != nil || cached || !result.valid() {
				t.Fatalf("finalize: result=%+v cached=%t err=%v", result, cached, err)
			}
			// Replay must use the receipt even if the old path now identifies
			// something else; it must neither reopen it nor repeat its metadata.
			platform.guardErr = errors.New("replay must not reacquire native authority")
			replayed, cached, err := finalizeNative(authority, t.Context(), claim)
			if err != nil || !cached || replayed != result {
				t.Fatalf("replay: result=%+v cached=%t err=%v", replayed, cached, err)
			}
			_, cached, err = materializeNative(authority, t.Context(), claim)
			if err != nil || !cached {
				t.Fatalf("materialization receipt: cached=%t err=%v", cached, err)
			}
			conflict := claim
			conflict.modified = mustModifiedTime(t, 99)
			if _, _, err := finalizeNative(authority, t.Context(), conflict); !errors.Is(err, ErrClaimConflict) {
				t.Fatalf("settled identity conflict = %v", err)
			}
			platform.guardErr = nil
			if authority.claims[claim.id].execution != nil {
				t.Fatal("receipt retained its execution and namespace snapshot")
			}
		}
		if got := platform.openDirectories.Load(); got != 1 {
			t.Fatalf("after subtree %d: open directories = %d, want only root", index, got)
		}
		late := mustClaim(t, authority, ClaimID(subtreeCount*2+2+index), branch.id, path+"/late", catalog.ModifiedTime{})
		if _, _, err := materializeNative(authority, t.Context(), late); !errors.Is(err, ErrParentUnavailable) {
			t.Fatalf("settled parent accepted new work: %v", err)
		}
	}
	if _, _, err := finalizeNative(authority, t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if got := platform.openDirectories.Load(); got != 0 {
		t.Fatalf("settled tree still owns %d directories", got)
	}
}

func TestFinalizationReleasePreservesRetryAndFailureSemantics(t *testing.T) {
	for _, outcome := range []string{"isolated", "ambiguous", "close failure", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			authority, platform := newTestAuthority(t, outputcap.AuthorityCreatedRoot, Config{})
			root := materializeRoot(t, authority, mustModifiedTime(t, 41))
			native := authority.claims[root.id].execution.retained.(*fakeDirectory)
			failure := errors.New("injected finalization failure")
			ctx := t.Context()
			switch outcome {
			case "isolated":
				platform.root.metadataAuthorityErr = failure
			case "ambiguous":
				platform.root.syncErr = failure
			case "close failure":
				native.closeErr = failure
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			result, _, err := finalizeNative(authority, ctx, root)
			if outcome == "canceled" {
				if !errors.Is(err, ErrNoMutation) || platform.openDirectories.Load() != 1 {
					t.Fatalf("retry authority lost: %v", err)
				}
				result, _, err = finalizeNative(authority, t.Context(), root)
			}
			if outcome == "ambiguous" || outcome == "close failure" {
				if !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, failure) {
					t.Fatalf("terminal failure = %v", err)
				}
				if _, cached, err := finalizeNative(authority, t.Context(), root); !cached || !errors.Is(err, ErrMutationAmbiguous) {
					t.Fatalf("ambiguous replay: cached=%t err=%v", cached, err)
				}
			} else if err != nil || !result.valid() {
				t.Fatalf("terminal result=%+v err=%v", result, err)
			}
			if got := platform.openDirectories.Load(); got != 0 {
				t.Fatalf("terminal finalization retained %d handles", got)
			}
			if err := authority.Close(); err != nil {
				t.Fatal(err)
			}
			if native.closeCalls != 1 {
				t.Fatalf("native close attempted %d times", native.closeCalls)
			}
		})
	}
}

func TestDirectoryWitnessBorrowPinsHandleUntilGuardCleanup(t *testing.T) {
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	root := materializeRoot(t, authority, catalog.ModifiedTime{})
	execution := authority.claims[root.id].execution
	_, cleanup, err := authority.openGuardedDirectory(root.id)
	if err != nil {
		t.Fatal(err)
	}
	if execution.gate.TryLock() {
		execution.gate.Unlock()
		t.Fatal("guarded operation did not pin its native witness")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if !execution.gate.TryLock() {
		t.Fatal("guard cleanup retained the witness borrow")
	}
	execution.gate.Unlock()
	lineage, err := authority.readyLineage(root.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := finalizeNative(authority, t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if release, err := borrowDirectoryLineage(lineage); !errors.Is(err, ErrParentUnavailable) {
		if release != nil {
			release()
		}
		t.Fatalf("stale borrower obtained retired authority: %v", err)
	}
	if got := platform.openDirectories.Load(); got != 0 {
		t.Fatalf("retired authority has %d open handles", got)
	}
}
