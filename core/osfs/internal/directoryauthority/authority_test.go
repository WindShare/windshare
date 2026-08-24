package directoryauthority

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func newTestAuthority(
	t *testing.T,
	disposition outputcap.RootOpenDisposition,
	config Config,
) (*Authority, *fakePlatform) {
	t.Helper()
	platform := newFakePlatform(disposition)
	authority, err := New(platform, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := authority.Close(); err != nil {
			t.Errorf("close authority: %v", err)
		}
	})
	return authority, platform
}

func mustModifiedTime(t *testing.T, seconds int64) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(seconds, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func mustClaim(
	t *testing.T,
	authority *Authority,
	id ClaimID,
	parentID ClaimID,
	path string,
	modified catalog.ModifiedTime,
) directoryClaim {
	t.Helper()
	locator, err := authority.canonicalLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := authority.newDirectoryClaim(id, parentID, locator, modified)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func materializeNative(
	authority *Authority,
	ctx context.Context,
	claim directoryClaim,
) (directoryMaterialization, bool, error) {
	authority.gate.RLock()
	defer authority.gate.RUnlock()
	return authority.materializeDirectory(ctx, claim)
}

func finalizeNative(
	authority *Authority,
	ctx context.Context,
	claim directoryClaim,
) (directoryFinalization, bool, error) {
	authority.gate.RLock()
	defer authority.gate.RUnlock()
	result, _, _, cached, err := authority.finalizeDirectory(ctx, claim)
	return result, cached, err
}

func materializeRoot(t *testing.T, authority *Authority, modified catalog.ModifiedTime) directoryClaim {
	t.Helper()
	claim := mustClaim(t, authority, 1, 0, "", modified)
	result, _, err := materializeNative(authority, context.Background(), claim)
	if err != nil || !result.valid() {
		t.Fatalf("materialize root result=%+v err=%v", result, err)
	}
	return claim
}

func TestCanonicalLocatorClaimsPlatformEquivalenceAndReservedNamespace(t *testing.T) {
	authority, _ := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})

	root, err := authority.CanonicalLocatorKey("")
	if err != nil || root != rootLocatorKey {
		t.Fatalf("root key=%q err=%v", root, err)
	}
	left, err := authority.CanonicalLocatorKey("Folder/Readme")
	if err != nil {
		t.Fatal(err)
	}
	right, err := authority.CanonicalLocatorKey("folder/README")
	if err != nil || left != right {
		t.Fatalf("platform equivalent keys left=%q right=%q err=%v", left, right, err)
	}
	locator, err := authority.canonicalLocator("Folder/Readme")
	if err != nil || locator.canonicalPath != "Folder/Readme" || locator.leaf != "Readme" {
		t.Fatalf("locator=%+v err=%v", locator, err)
	}

	for _, path := range []string{
		".windshare-output", ".WINDSHARE-OUTPUT/checkpoints-v1", ".windshare-output.probe-token/item",
	} {
		if _, err := authority.CanonicalLocatorKey(path); !errors.Is(err, outputfault.ErrReservedPath) {
			t.Errorf("reserved path %q error=%v", path, err)
		}
	}
	for _, path := range []string{"folder//child", "folder/../child", "/absolute"} {
		if _, err := authority.CanonicalLocatorKey(path); !errors.Is(err, ErrInvalidLocator) {
			t.Errorf("invalid path %q error=%v", path, err)
		}
	}
}

func TestAuthorityRejectsInvalidConfigurationAndClosedUse(t *testing.T) {
	if _, err := New(nil, Config{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil platform error=%v", err)
	}
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	if _, err := New(platform, Config{ParentSnapshotEntryLimit: catalog.MaxDirectoryEntries + 1}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("oversized snapshot error=%v", err)
	}
	invalidDisposition := newFakePlatform(outputcap.RootOpenDisposition(""))
	if _, err := New(invalidDisposition, Config{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid disposition error=%v", err)
	}

	authority, err := New(platform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.CanonicalLocatorKey(""); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed canonicalizer error=%v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
}

func TestRootDispositionControlsMetadataAuthority(t *testing.T) {
	modified := mustModifiedTime(t, 17)
	tests := []struct {
		name            string
		platform        outputcap.RootOpenDisposition
		wantDisposition DirectoryDisposition
		wantSetCalls    int
	}{
		{"caller container", outputcap.CallerProvidedContainer, DirectoryCallerProvidedRoot, 0},
		{"authority created", outputcap.AuthorityCreatedRoot, DirectoryAuthorityCreatedRoot, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority, platform := newTestAuthority(t, test.platform, Config{})
			root := mustClaim(t, authority, 1, 0, "", modified)
			materialized, _, err := materializeNative(authority, context.Background(), root)
			if err != nil || materialized.disposition != test.wantDisposition {
				t.Fatalf("materialization=%+v err=%v", materialized, err)
			}
			finalized, cached, err := finalizeNative(authority, context.Background(), root)
			if err != nil || cached || finalized.kind != outputsession.DirectoryFinalizationFinalized {
				t.Fatalf("finalization=%+v cached=%t err=%v", finalized, cached, err)
			}
			platform.mu.Lock()
			setCalls, syncCalls := platform.root.setCalls, platform.root.syncCalls
			platform.mu.Unlock()
			if setCalls != test.wantSetCalls || syncCalls == 0 {
				t.Fatalf("metadata calls set=%d sync=%d", setCalls, syncCalls)
			}
			_, cached, err = finalizeNative(authority, context.Background(), root)
			if err != nil || !cached {
				t.Fatalf("cached finalization cached=%t err=%v", cached, err)
			}
		})
	}
}

func TestParentSnapshotRejectsAliasesAndPostSnapshotCollision(t *testing.T) {
	t.Run("immutable alias index", func(t *testing.T) {
		platform := newFakePlatform(outputcap.CallerProvidedContainer)
		platform.addDirectory(platform.rootNode(), "LongFolder", "LONGFO~1")
		snapshotter := &fakeAliasSnapshotter{entries: []PublicEntryName{{
			Name: "LongFolder", Aliases: []string{"LONGFO~1"},
		}}}
		authority, err := New(platform, Config{Snapshotter: snapshotter, ParentSnapshotEntryLimit: 8})
		if err != nil {
			t.Fatal(err)
		}
		defer authority.Close()
		materializeRoot(t, authority, catalog.ModifiedTime{})
		claim := mustClaim(t, authority, 2, 1, "longfo~1", catalog.ModifiedTime{})
		_, _, err = materializeNative(authority, context.Background(), claim)
		if !errors.Is(err, ErrPlatformEquivalentLocator) || !errors.Is(err, ErrNoMutation) {
			t.Fatalf("alias collision error=%v", err)
		}
		if snapshotter.count() != 1 {
			t.Fatalf("snapshot calls=%d", snapshotter.count())
		}
	})

	t.Run("guarded exact race", func(t *testing.T) {
		platform := newFakePlatform(outputcap.CallerProvidedContainer)
		snapshotter := &fakeAliasSnapshotter{}
		snapshotter.after = func() {
			platform.addDirectory(platform.rootNode(), "RacingFolder", "RACING~1")
		}
		authority, err := New(platform, Config{Snapshotter: snapshotter, ParentSnapshotEntryLimit: 8})
		if err != nil {
			t.Fatal(err)
		}
		defer authority.Close()
		materializeRoot(t, authority, catalog.ModifiedTime{})
		claim := mustClaim(t, authority, 2, 1, "racing~1", catalog.ModifiedTime{})
		_, _, err = materializeNative(authority, context.Background(), claim)
		if !errors.Is(err, ErrPlatformEquivalentLocator) || !errors.Is(err, ErrEntryCollision) {
			t.Fatalf("post-snapshot collision error=%v", err)
		}
	})

	t.Run("exact existing descendant becomes an unowned container", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		existing := platform.addDirectory(platform.rootNode(), "existing")
		materializeRoot(t, authority, catalog.ModifiedTime{})
		modified := mustModifiedTime(t, 29)
		claim := mustClaim(t, authority, 2, 1, "existing", modified)
		result, _, err := materializeNative(authority, context.Background(), claim)
		if err != nil || result.disposition != DirectoryPreexistingDescendant {
			t.Fatalf("existing descendant result=%+v error=%v", result, err)
		}
		if _, _, err := finalizeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		child := mustClaim(t, authority, 3, 2, "existing/child", catalog.ModifiedTime{})
		if childResult, _, err := materializeNative(authority, context.Background(), child); err != nil ||
			childResult.disposition != DirectoryAuthorityCreatedDescendant {
			t.Fatalf("child result=%+v error=%v", childResult, err)
		}
		platform.mu.Lock()
		setCalls, syncCalls, createCalls := existing.setCalls, existing.syncCalls, existing.createCalls
		platform.mu.Unlock()
		if setCalls != 0 || syncCalls == 0 || createCalls != 1 {
			t.Fatalf("preexisting authority set=%d sync=%d creates=%d", setCalls, syncCalls, createCalls)
		}
	})

	t.Run("existing non-directory remains a collision", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		platform.addFile(platform.rootNode(), "existing", 1)
		materializeRoot(t, authority, catalog.ModifiedTime{})
		claim := mustClaim(t, authority, 2, 1, "existing", catalog.ModifiedTime{})
		_, _, err := materializeNative(authority, context.Background(), claim)
		if !errors.Is(err, ErrEntryCollision) || !errors.Is(err, ErrNoMutation) {
			t.Fatalf("existing file error=%v", err)
		}
	})
}

func TestPreexistingDescendantGuardRejectsReplacement(t *testing.T) {
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	platform.addDirectory(platform.rootNode(), "existing")
	materializeRoot(t, authority, catalog.ModifiedTime{})
	claim := mustClaim(t, authority, 2, 1, "existing", catalog.ModifiedTime{})
	if result, _, err := materializeNative(authority, context.Background(), claim); err != nil ||
		result.disposition != DirectoryPreexistingDescendant {
		t.Fatalf("existing descendant result=%+v error=%v", result, err)
	}
	replacement := platform.replaceDirectory(platform.rootNode(), "existing")
	child := mustClaim(t, authority, 3, 2, "existing/child", catalog.ModifiedTime{})
	_, _, err := materializeNative(authority, context.Background(), child)
	if !errors.Is(err, ErrRetainedAuthorityChanged) || !errors.Is(err, ErrNoMutation) {
		t.Fatalf("replaced preexisting ancestor error=%v", err)
	}
	platform.mu.Lock()
	createCalls := replacement.createCalls
	platform.mu.Unlock()
	if createCalls != 0 {
		t.Fatalf("replacement received %d mutations", createCalls)
	}
}

func TestConcurrentChildrenEnumerateParentOnceWithinBound(t *testing.T) {
	const (
		childCount    = 64
		snapshotLimit = 128
	)
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{
		ParentSnapshotEntryLimit: snapshotLimit,
	})
	materializeRoot(t, authority, catalog.ModifiedTime{})

	claims := make([]directoryClaim, childCount)
	for index := range claims {
		claims[index] = mustClaim(
			t, authority, ClaimID(index+2), 1, fmt.Sprintf("child-%03d", index), catalog.ModifiedTime{},
		)
	}
	var wait sync.WaitGroup
	errorsByClaim := make(chan error, childCount)
	for _, claim := range claims {
		wait.Go(func() {
			result, _, err := materializeNative(authority, context.Background(), claim)
			if err == nil && result.disposition != DirectoryAuthorityCreatedDescendant {
				err = fmt.Errorf("unexpected disposition %d", result.disposition)
			}
			errorsByClaim <- err
		})
	}
	wait.Wait()
	close(errorsByClaim)
	for err := range errorsByClaim {
		if err != nil {
			t.Fatal(err)
		}
	}

	platform.mu.Lock()
	namesCalls := platform.root.namesCalls
	lastLimit := platform.root.lastNamesLimit
	createCalls := platform.root.createCalls
	platform.mu.Unlock()
	if namesCalls != 1 || lastLimit != snapshotLimit || createCalls != childCount {
		t.Fatalf("bounded work names=%d limit=%d creates=%d", namesCalls, lastLimit, createCalls)
	}
}

func TestRetainedGuardRejectsReplacedAncestor(t *testing.T) {
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	root := materializeRoot(t, authority, catalog.ModifiedTime{})
	child := mustClaim(t, authority, 2, root.id, "child", catalog.ModifiedTime{})
	if _, _, err := materializeNative(authority, context.Background(), child); err != nil {
		t.Fatal(err)
	}
	replacement := platform.replaceDirectory(platform.rootNode(), "child")
	grandchild := mustClaim(t, authority, 3, child.id, "child/grandchild", catalog.ModifiedTime{})
	_, _, err := materializeNative(authority, context.Background(), grandchild)
	if !errors.Is(err, ErrRetainedAuthorityChanged) || !errors.Is(err, ErrNoMutation) {
		t.Fatalf("replaced ancestor error=%v", err)
	}
	platform.mu.Lock()
	createCalls := replacement.createCalls
	platform.mu.Unlock()
	if createCalls != 0 {
		t.Fatalf("replacement received %d mutations", createCalls)
	}
}

func TestCreateMutationReconciliation(t *testing.T) {
	reported := errors.New("reported create failure")
	tests := []struct {
		name           string
		plan           fakeCreatePlan
		wantError      error
		wantReconciled bool
		wantRetry      bool
	}{
		{"returned handle proves creation", fakeCreatePlan{err: reported, mutate: true, returnHandle: true}, nil, true, false},
		{"absent proves no mutation", fakeCreatePlan{err: reported}, ErrNoMutation, false, true},
		{"unwitnessed creation is ambiguous", fakeCreatePlan{err: reported, mutate: true}, ErrMutationAmbiguous, false, false},
		{"reported collision stays no mutation", fakeCreatePlan{err: outputcap.ErrNamespaceCollision, mutate: true}, ErrNoMutation, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
			materializeRoot(t, authority, catalog.ModifiedTime{})
			platform.mu.Lock()
			plan := test.plan
			platform.root.nextCreate = &plan
			platform.mu.Unlock()
			claim := mustClaim(t, authority, 2, 1, "child", catalog.ModifiedTime{})
			result, _, err := materializeNative(authority, context.Background(), claim)
			if test.wantError == nil {
				if err != nil || !result.valid() || result.reconciled != test.wantReconciled {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, test.wantError) {
				t.Fatalf("error=%v want %v", err, test.wantError)
			}
			if test.wantRetry {
				result, _, err = materializeNative(authority, context.Background(), claim)
				if err != nil || !result.valid() {
					t.Fatalf("retry result=%+v err=%v", result, err)
				}
			}
			if errors.Is(test.wantError, ErrMutationAmbiguous) {
				_, _, retryErr := materializeNative(authority, context.Background(), claim)
				if !errors.Is(retryErr, ErrMutationAmbiguous) {
					t.Fatalf("ambiguous retry error=%v", retryErr)
				}
			}
		})
	}
}

func TestExecutorBoundaryReportsNoChangeForUnboundClaims(t *testing.T) {
	authority, _ := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	materialization, err := authority.MaterializeDirectory(context.Background(), outputsession.DirectoryClaim{})
	if !errors.Is(err, ErrNoMutation) || materialization.Cut != outputsession.MutationNoChange {
		t.Fatalf("materialization=%+v err=%v", materialization, err)
	}
	finalization, err := authority.FinalizeDirectory(context.Background(), outputsession.DirectoryClaim{})
	if !errors.Is(err, ErrNoMutation) || finalization.Cut != outputsession.MutationNoChange {
		t.Fatalf("finalization=%+v err=%v", finalization, err)
	}
	var nilAuthority *Authority
	materialization, err = nilAuthority.MaterializeDirectory(context.Background(), outputsession.DirectoryClaim{})
	if !errors.Is(err, ErrNoMutation) || materialization.Cut != outputsession.MutationNoChange {
		t.Fatalf("nil materialization=%+v err=%v", materialization, err)
	}
	finalization, err = nilAuthority.FinalizeDirectory(context.Background(), outputsession.DirectoryClaim{})
	if !errors.Is(err, ErrNoMutation) || finalization.Cut != outputsession.MutationNoChange {
		t.Fatalf("nil finalization=%+v err=%v", finalization, err)
	}
}

func TestMaterializationPreMutationFailuresCanRetry(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		authority, _ := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		claim := mustClaim(t, authority, 1, 0, "", catalog.ModifiedTime{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := materializeNative(authority, ctx, claim); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrNoMutation) {
			t.Fatalf("cancelled materialization error=%v", err)
		}
		if result, _, err := materializeNative(authority, context.Background(), claim); err != nil || !result.valid() {
			t.Fatalf("retry result=%+v err=%v", result, err)
		}
	})

	t.Run("invalid metadata", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		claim := mustClaim(t, authority, 1, 0, "", catalog.ModifiedTime{})
		platform.modifiedErr = errors.New("metadata unsupported")
		if _, _, err := materializeNative(authority, context.Background(), claim); !errors.Is(err, ErrNoMutation) {
			t.Fatalf("metadata validation error=%v", err)
		}
		platform.modifiedErr = nil
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatalf("metadata retry error=%v", err)
		}
	})

	t.Run("root guard unavailable", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		claim := mustClaim(t, authority, 1, 0, "", catalog.ModifiedTime{})
		platform.guardErr = errors.New("guard unavailable")
		if _, _, err := materializeNative(authority, context.Background(), claim); !errors.Is(err, ErrNoMutation) {
			t.Fatalf("guard error=%v", err)
		}
		platform.guardErr = nil
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatalf("guard retry error=%v", err)
		}
	})

	t.Run("root guard close failure", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		claim := mustClaim(t, authority, 1, 0, "", catalog.ModifiedTime{})
		platform.guardCloseErr = errors.New("guard close failed")
		if _, _, err := materializeNative(authority, context.Background(), claim); !errors.Is(err, ErrNoMutation) {
			t.Fatalf("guard close error=%v", err)
		}
		platform.guardCloseErr = nil
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatalf("guard close retry error=%v", err)
		}
	})

	t.Run("parent and claim binding", func(t *testing.T) {
		authority, _ := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		missingParent := mustClaim(t, authority, 2, 1, "child", catalog.ModifiedTime{})
		if _, _, err := materializeNative(authority, context.Background(), missingParent); !errors.Is(err, ErrParentUnavailable) {
			t.Fatalf("missing parent error=%v", err)
		}
		root := materializeRoot(t, authority, catalog.ModifiedTime{})
		secondRoot := mustClaim(t, authority, 3, 0, "", catalog.ModifiedTime{})
		if _, _, err := materializeNative(authority, context.Background(), secondRoot); !errors.Is(err, ErrClaimConflict) {
			t.Fatalf("second root error=%v", err)
		}
		conflict := mustClaim(t, authority, root.id, 0, "", mustModifiedTime(t, 9))
		if _, _, err := materializeNative(authority, context.Background(), conflict); !errors.Is(err, ErrClaimConflict) {
			t.Fatalf("claim conflict error=%v", err)
		}
		if result, cached, err := materializeNative(authority, context.Background(), root); err != nil || !cached || !result.valid() {
			t.Fatalf("cached root result=%+v cached=%t err=%v", result, cached, err)
		}
	})

	t.Run("create authority refusal", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		materializeRoot(t, authority, catalog.ModifiedTime{})
		platform.root.createAuthorityErr = errors.New("create authority changed")
		claim := mustClaim(t, authority, 2, 1, "child", catalog.ModifiedTime{})
		if _, _, err := materializeNative(authority, context.Background(), claim); !errors.Is(err, ErrNoMutation) {
			t.Fatalf("create refusal error=%v", err)
		}
		platform.root.createAuthorityErr = nil
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatalf("create retry error=%v", err)
		}
	})
}

func TestParentSnapshotValidationIsBoundedAndSticky(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		entries []PublicEntryName
		want    error
	}{
		{"entry overflow", 1, []PublicEntryName{{Name: "one"}, {Name: "two"}}, ErrParentSnapshotUnavailable},
		{"empty long name", 8, []PublicEntryName{{}}, ErrParentSnapshotUnavailable},
		{"alias overflow", 8, []PublicEntryName{{Name: "long", Aliases: []string{"a", "b"}}}, ErrParentSnapshotUnavailable},
		{"equivalent owners", 8, []PublicEntryName{{Name: "Alpha"}, {Name: "alpha"}}, ErrPlatformEquivalentLocator},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := newFakePlatform(outputcap.CallerProvidedContainer)
			snapshotter := &fakeAliasSnapshotter{entries: test.entries}
			authority, err := New(platform, Config{
				Snapshotter: snapshotter, ParentSnapshotEntryLimit: test.limit,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer authority.Close()
			materializeRoot(t, authority, catalog.ModifiedTime{})
			for index := range 2 {
				claim := mustClaim(t, authority, ClaimID(index+2), 1, fmt.Sprintf("candidate-%d", index), catalog.ModifiedTime{})
				if _, _, err := materializeNative(authority, context.Background(), claim); !errors.Is(err, test.want) {
					t.Fatalf("snapshot error=%v want %v", err, test.want)
				}
			}
			if snapshotter.count() != 1 {
				t.Fatalf("snapshot calls=%d", snapshotter.count())
			}
		})
	}
}

func TestSnapshotCollisionIsOnlyDecisionAidAfterRemoval(t *testing.T) {
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	platform.addDirectory(platform.rootNode(), "OldFolder", "OLD~1")
	snapshotter := &fakeAliasSnapshotter{entries: []PublicEntryName{{
		Name: "OldFolder", Aliases: []string{"OLD~1"},
	}}}
	authority, err := New(platform, Config{Snapshotter: snapshotter, ParentSnapshotEntryLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	materializeRoot(t, authority, catalog.ModifiedTime{})
	seed := mustClaim(t, authority, 2, 1, "seed", catalog.ModifiedTime{})
	if _, _, err := materializeNative(authority, context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	platform.mu.Lock()
	delete(platform.root.entries, "OldFolder")
	platform.mu.Unlock()
	claim := mustClaim(t, authority, 3, 1, "old~1", catalog.ModifiedTime{})
	result, _, err := materializeNative(authority, context.Background(), claim)
	if err != nil || result.disposition != DirectoryAuthorityCreatedDescendant {
		t.Fatalf("stale snapshot result=%+v err=%v", result, err)
	}
}

func TestGuardCleanupAfterCreationIsMutationAmbiguous(t *testing.T) {
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	materializeRoot(t, authority, catalog.ModifiedTime{})
	authority.mu.Lock()
	rootRecord := authority.claims[1]
	authority.mu.Unlock()
	if _, err := authority.parentSnapshot(rootRecord); err != nil {
		t.Fatal(err)
	}
	platform.guardCloseErr = errors.New("guard close failed")
	claim := mustClaim(t, authority, 2, 1, "child", catalog.ModifiedTime{})
	_, _, err := materializeNative(authority, context.Background(), claim)
	if !errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("guard cleanup error=%v", err)
	}
}

func TestGuardCleanupAfterPreexistingOpenIsNoMutation(t *testing.T) {
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	platform.addDirectory(platform.rootNode(), "existing")
	materializeRoot(t, authority, catalog.ModifiedTime{})
	authority.mu.Lock()
	rootRecord := authority.claims[1]
	authority.mu.Unlock()
	if _, err := authority.parentSnapshot(rootRecord); err != nil {
		t.Fatal(err)
	}
	platform.guardCloseErr = errors.New("guard close failed")
	claim := mustClaim(t, authority, 2, 1, "existing", catalog.ModifiedTime{})
	_, _, err := materializeNative(authority, context.Background(), claim)
	if !errors.Is(err, ErrNoMutation) || errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("preexisting guard cleanup error=%v", err)
	}
	platform.guardCloseErr = nil
	result, _, err := materializeNative(authority, context.Background(), claim)
	if err != nil || result.disposition != DirectoryPreexistingDescendant {
		t.Fatalf("retry result=%+v error=%v", result, err)
	}
}

func TestFinalizationReconcilesMetadataAndCachesSettlement(t *testing.T) {
	authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	materializeRoot(t, authority, catalog.ModifiedTime{})
	reported := errors.New("reported metadata failure")

	t.Run("reported error but metadata matches", func(t *testing.T) {
		modified := mustModifiedTime(t, 21)
		claim := mustClaim(t, authority, 2, 1, "matching", modified)
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		node := platform.root.entries["matching"].node
		node.setErr, node.setMutates = reported, true
		platform.mu.Unlock()
		result, cached, err := finalizeNative(authority, context.Background(), claim)
		if err != nil || cached || result.kind != outputsession.DirectoryFinalizationFinalized || !result.reconciled {
			t.Fatalf("result=%+v cached=%t err=%v", result, cached, err)
		}
		platform.mu.Lock()
		setCalls := node.setCalls
		platform.mu.Unlock()
		if _, cached, err = finalizeNative(authority, context.Background(), claim); err != nil || !cached {
			t.Fatalf("cached result cached=%t err=%v", cached, err)
		}
		platform.mu.Lock()
		defer platform.mu.Unlock()
		if node.setCalls != setCalls {
			t.Fatalf("cached finalization repeated metadata: %d -> %d", setCalls, node.setCalls)
		}
	})

	t.Run("stable mismatch isolates leaf", func(t *testing.T) {
		modified := mustModifiedTime(t, 22)
		claim := mustClaim(t, authority, 3, 1, "mismatch", modified)
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		node := platform.root.entries["mismatch"].node
		node.setErr = reported
		platform.mu.Unlock()
		result, _, err := finalizeNative(authority, context.Background(), claim)
		if err != nil || result.kind != outputsession.DirectoryFinalizationIsolatedFailure || !result.failure.Valid() {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("authority refusal isolates before mutation", func(t *testing.T) {
		modified := mustModifiedTime(t, 23)
		claim := mustClaim(t, authority, 4, 1, "refused", modified)
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		node := platform.root.entries["refused"].node
		node.metadataAuthorityErr = reported
		platform.mu.Unlock()
		result, _, err := finalizeNative(authority, context.Background(), claim)
		if err != nil || result.kind != outputsession.DirectoryFinalizationIsolatedFailure {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		platform.mu.Lock()
		defer platform.mu.Unlock()
		if node.setCalls != 0 {
			t.Fatalf("metadata setter called %d times", node.setCalls)
		}
	})

	t.Run("unobservable outcome is ambiguous", func(t *testing.T) {
		modified := mustModifiedTime(t, 24)
		claim := mustClaim(t, authority, 5, 1, "ambiguous", modified)
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		node := platform.root.entries["ambiguous"].node
		node.setErr = reported
		node.metadataObserveErr = errors.New("observation failed")
		platform.mu.Unlock()
		_, _, err := finalizeNative(authority, context.Background(), claim)
		if !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrMetadataReconcile) {
			t.Fatalf("ambiguous metadata error=%v", err)
		}
	})
}

func TestFinalizationCancellationBeforeMutationCanRetry(t *testing.T) {
	authority, _ := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
	materializeRoot(t, authority, catalog.ModifiedTime{})
	claim := mustClaim(t, authority, 2, 1, "child", mustModifiedTime(t, 30))
	if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := finalizeNative(authority, cancelled, claim)
	if !errors.Is(err, ErrNoMutation) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled finalization error=%v", err)
	}
	result, _, err := finalizeNative(authority, context.Background(), claim)
	if err != nil || result.kind != outputsession.DirectoryFinalizationFinalized {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
}

func TestFinalizationRevalidatesRetainedAuthorityAndClaimBinding(t *testing.T) {
	t.Run("replaced directory", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		materializeRoot(t, authority, catalog.ModifiedTime{})
		claim := mustClaim(t, authority, 2, 1, "child", mustModifiedTime(t, 31))
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.replaceDirectory(platform.rootNode(), "child")
		if _, _, err := finalizeNative(authority, context.Background(), claim); !errors.Is(err, ErrRetainedAuthorityChanged) || !errors.Is(err, ErrMutationAmbiguous) {
			t.Fatalf("replaced directory error=%v", err)
		}
		if _, _, err := finalizeNative(authority, context.Background(), claim); !errors.Is(err, ErrMutationAmbiguous) {
			t.Fatalf("ambiguous retry error=%v", err)
		}
	})

	t.Run("missing and conflicting claims", func(t *testing.T) {
		authority, _ := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		root := materializeRoot(t, authority, catalog.ModifiedTime{})
		missing := mustClaim(t, authority, 2, root.id, "missing", catalog.ModifiedTime{})
		if _, _, err := finalizeNative(authority, context.Background(), missing); !errors.Is(err, ErrParentUnavailable) {
			t.Fatalf("missing claim error=%v", err)
		}
		conflict := mustClaim(t, authority, root.id, 0, "", mustModifiedTime(t, 32))
		if _, _, err := finalizeNative(authority, context.Background(), conflict); !errors.Is(err, ErrClaimConflict) {
			t.Fatalf("conflicting claim error=%v", err)
		}
	})

	t.Run("durability failure after metadata", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		materializeRoot(t, authority, catalog.ModifiedTime{})
		claim := mustClaim(t, authority, 2, 1, "child", mustModifiedTime(t, 33))
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		platform.root.entries["child"].node.syncErr = errors.New("sync failed")
		platform.mu.Unlock()
		if _, _, err := finalizeNative(authority, context.Background(), claim); !errors.Is(err, ErrMutationAmbiguous) {
			t.Fatalf("sync error=%v", err)
		}
	})

	t.Run("guard cleanup after metadata", func(t *testing.T) {
		authority, platform := newTestAuthority(t, outputcap.CallerProvidedContainer, Config{})
		materializeRoot(t, authority, catalog.ModifiedTime{})
		claim := mustClaim(t, authority, 2, 1, "child", mustModifiedTime(t, 34))
		if _, _, err := materializeNative(authority, context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		platform.guardCloseErr = errors.New("guard close failed")
		if _, _, err := finalizeNative(authority, context.Background(), claim); !errors.Is(err, ErrMutationAmbiguous) {
			t.Fatalf("guard cleanup error=%v", err)
		}
	})
}

type rejectingFileExecutor struct{}

func (rejectingFileExecutor) BeginFile(
	context.Context,
	outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, errFakeUnsupported
}

type identityValue interface {
	~[catalog.IdentityBytes]byte
}

func testIdentity[T identityValue](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}

func testDirectTreeIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules transfer.SelectionRules,
) transfer.ReceiveIntent {
	t.Helper()
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operationRaw := testIdentity[receivecontract.OperationID](221)
	operation, err := receivecontract.OperationIDFromBytes(operationRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	reservationRaw := testIdentity[receivecontract.DestinationReservationID](231)
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(reservationRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	authorityRaw := bytes.Repeat([]byte{0xd1}, receivecontract.AuthorityRefBytes)
	authority, err := receivecontract.AuthorityRefFromBytes(authorityRaw)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func testSourceDirectory(
	t *testing.T,
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	parent transfer.DirectoryAdmission,
	path string,
	modified catalog.ModifiedTime,
) transfer.AuthenticatedSourceDirectory {
	t.Helper()
	sourcePath := ordinaryoutput.EmptySourceCatalogPath()
	if path != "" {
		var err error
		sourcePath, err = ordinaryoutput.NewSourceCatalogPath(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	return transfer.AuthenticatedSourceDirectory{
		DirectoryID: directory, Generation: generation, ParentAdmission: parent,
		SourcePath: sourcePath, ModifiedTime: modified,
	}
}

func projectedDirectoryRequest(
	t *testing.T,
	intent transfer.ReceiveIntent,
	directory transfer.AuthenticatedSourceDirectory,
	parent transfer.MaterializedDirectoryClaim,
) transfer.DirectoryMaterializationRequest {
	t.Helper()
	request, err := transfer.NewDirectoryMaterializationRequest(
		intent, directory, ordinaryoutput.SourceNodeSelected, parent,
	)
	if err != nil {
		request, err = transfer.NewDirectoryMaterializationRequest(
			intent, directory, ordinaryoutput.SourceNodeConnectsSelection, parent,
		)
	}
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestOutputSessionUsesAuthorityDirectlyWithoutCompositionAdapter(t *testing.T) {
	var authority *Authority
	var traceCalls atomic.Int64
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	configured, err := New(platform, Config{Trace: func(TraceEvent) {
		traceCalls.Add(1)
		// Re-entry proves trace delivery is outside native-authority locks.
		if _, canonicalErr := authority.CanonicalLocatorKey(""); canonicalErr != nil {
			t.Errorf("trace re-entry: %v", canonicalErr)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	authority = configured
	defer authority.Close()

	share := testIdentity[catalog.ShareInstance](1)
	rootID := testIdentity[catalog.DirectoryID](21)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent := testDirectTreeIntent(t, share, rootID, rules)
	sessionID := testIdentity[transfer.OutputSessionID](61)
	secret := bytes.Repeat([]byte{0x5a}, 32)
	session, err := outputsession.New(outputsession.Config{
		Intent: intent, SessionID: sessionID,
		Capabilities: transfer.DirectTreeCapabilities{
			Durability:  transfer.DurabilityPowerLoss,
			RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		},
		ReceiptSecret: secret,
		Locator:       authority,
		Destinations: outputsession.ArtifactDestinationBinderFunc(func(path ordinaryoutput.ArtifactPath) (outputsession.DestinationPath, error) {
			return outputsession.NewDestinationPath(path.String())
		}),
		Directories: authority, Files: rejectingFileExecutor{},
		Resources: outputsession.ResourceReleaserFunc(func(context.Context) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		root := testSourceDirectory(
			t, rootID, testIdentity[catalog.DirectoryGeneration](31),
			transfer.DirectoryAdmission{}, "", catalog.ModifiedTime{},
		)
		rootAdmission, admitErr := session.AdmitDirectory(
			ctx, projectedDirectoryRequest(t, intent, root, transfer.MaterializedDirectoryClaim{}),
		)
		if admitErr != nil {
			done <- admitErr
			return
		}
		child := testSourceDirectory(
			t, testIdentity[catalog.DirectoryID](41), testIdentity[catalog.DirectoryGeneration](42),
			rootAdmission, "child", mustModifiedTime(t, 51),
		)
		childAdmission, admitErr := session.AdmitDirectory(
			ctx, projectedDirectoryRequest(t, intent, child, transfer.MaterializedDirectoryClaim{}),
		)
		if admitErr == nil {
			platform.mu.Lock()
			platform.root.entries["child"].node.setErr = errors.New("metadata rejected")
			platform.mu.Unlock()
			var settlement transfer.DirectorySettlement
			settlement, admitErr = session.FinalizeDirectory(ctx, childAdmission)
			if admitErr == nil && settlement.Kind() != transfer.DirectoryIsolatedFailure {
				admitErr = fmt.Errorf("child settlement kind=%d", settlement.Kind())
			}
		}
		if admitErr == nil {
			_, admitErr = session.FinalizeDirectory(ctx, rootAdmission)
		}
		done <- admitErr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("direct outputsession integration deadlocked")
	}
	if traceCalls.Load() != 2 {
		t.Fatalf("trace calls=%d want 2 materialized-directory operations", traceCalls.Load())
	}
}
