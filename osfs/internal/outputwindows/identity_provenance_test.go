//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

type windowsCountingObjectIDProvider struct {
	identity windowsPersistentObjectID
	err      error
	calls    atomic.Int64
}

func (provider *windowsCountingObjectIDProvider) CreateOrGet(
	windows.Handle,
) (windowsPersistentObjectID, error) {
	provider.calls.Add(1)
	return provider.identity, provider.err
}

func windowsOpenGuardedTestRoot(t *testing.T) (*nativeOutputPlatform, *windowsPublicOperationGuard) {
	t.Helper()
	platform, err := openWindowsOutputPlatform(windowsNativeTestTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close native test platform: %v", err)
		}
	})
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close native ancestry guard: %v", err)
		}
	})
	return platform, guard
}

func TestWindowsIdentityPreparationIsTheOnlyObjectIDMutationBoundary(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := guard.Root()
	provider := &windowsCountingObjectIDProvider{identity: windowsPersistentObjectID{0x41}}
	root.objectIDs = provider
	root.objectIDState = newWindowsPersistentObjectIDState()

	if _, err := root.identityClaim(); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("unprepared read-only claim error = %v", err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("read-only claim invoked CreateOrGet %d times", calls)
	}
	claim, err := root.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if len(claim) == 0 || len(claim) > windowsDirectoryClaimMaxBytes {
		t.Fatalf("prepared claim length = %d", len(claim))
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("first preparation invoked CreateOrGet %d times", calls)
	}
	repeated, err := root.identityClaim()
	if err != nil || !bytes.Equal(claim, repeated) {
		t.Fatalf("read-only claim differs after preparation: equal=%t error=%v", bytes.Equal(claim, repeated), err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("read-only prepared claim invoked CreateOrGet %d times", calls)
	}
	preparedAgain, err := root.prepareIdentityClaim()
	if err != nil || !bytes.Equal(claim, preparedAgain) {
		t.Fatalf("idempotent preparation differs: equal=%t error=%v", bytes.Equal(claim, preparedAgain), err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("second preparation invoked CreateOrGet %d times", calls)
	}
}

func TestWindowsIdentityPreparationValidatesBeforeAndAfterCreateOrGet(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := guard.Root()
	provider := &windowsCountingObjectIDProvider{identity: windowsPersistentObjectID{0x42}}
	root.objectIDs = provider
	root.objectIDState = newWindowsPersistentObjectIDState()

	injected := errors.New("injected ancestry authority failure")
	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error { return injected })
	if _, err := root.prepareIdentityClaim(); !errors.Is(err, errWindowsOutputUnsafe) || !errors.Is(err, injected) {
		t.Fatalf("pre-authority failure = %v", err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("failed pre-authority check invoked CreateOrGet %d times", calls)
	}

	nativeInspector := nativeWindowsObjectInspector{}
	facts, err := nativeInspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	var inspections atomic.Int64
	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	root.inspector = windowsObjectInspectorFunc(func(windows.Handle) (windowsObjectFacts, error) {
		if inspections.Add(1) >= 3 {
			return windowsObjectFacts{}, nil
		}
		return facts, nil
	})
	_, err = root.prepareIdentityClaim()
	mapped := windowsOutputError(err)
	if !errors.Is(err, errWindowsOutputUnsafe) ||
		errors.Is(err, outputfault.ErrAncestryAuthorityDenied) ||
		!errors.Is(mapped, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("post-CreateOrGet incarnation failure = %v", mapped)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("post-validation failure CreateOrGet calls = %d", calls)
	}
	root.inspector = nativeInspector
	if identity, prepared, identityErr := root.cachedPersistentObjectID(); identityErr != nil || prepared || identity.valid() {
		t.Fatalf("post-validation failure published identity=%x prepared=%t error=%v", identity, prepared, identityErr)
	}
}

func TestWindowsReadOnlyIdentityClaimPreservesAuthorityTraceTaxonomy(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := guard.Root()
	provider := &windowsCountingObjectIDProvider{identity: windowsPersistentObjectID{0x45}}
	root.objectIDs = provider
	root.objectIDState = newWindowsPersistentObjectIDState()
	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	if _, err := root.prepareIdentityClaim(); err != nil {
		t.Fatal(err)
	}

	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error {
		return errors.Join(errWindowsOutputUnsupported, errors.New("injected ACL ambiguity"))
	})
	if _, err := root.identityClaim(); !errors.Is(err, errWindowsOutputUnsupported) {
		t.Fatalf("read-only authority trace taxonomy = %v", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("read-only authority denial invoked CreateOrGet %d times", calls)
	}

	structural := errors.New("injected handle inspection failure")
	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	root.inspector = windowsObjectInspectorFunc(func(windows.Handle) (windowsObjectFacts, error) {
		return windowsObjectFacts{}, structural
	})
	_, err := root.identityClaim()
	mapped := windowsOutputError(err)
	if !errors.Is(err, structural) ||
		errors.Is(err, outputfault.ErrAncestryAuthorityDenied) ||
		!errors.Is(mapped, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("read-only structural trace taxonomy = %v", mapped)
	}
}

func TestWindowsIdentityClaimsAreRaceSafeAndDeterministic(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := guard.Root()
	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	provider := &windowsCountingObjectIDProvider{identity: windowsPersistentObjectID{0x43}}
	root.objectIDs = provider
	root.objectIDState = newWindowsPersistentObjectIDState()
	want, err := root.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}

	const workers = 48
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Add(1)
		go func(prepare bool) {
			defer group.Done()
			var claim []byte
			var err error
			if prepare {
				claim, err = root.prepareIdentityClaim()
			} else {
				claim, err = root.identityClaim()
			}
			if err != nil || !bytes.Equal(claim, want) {
				errorsByWorker <- fmt.Errorf("claim equal=%t: %w", bytes.Equal(claim, want), err)
			}
		}(index%2 == 0)
	}
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Error(err)
	}
}

func TestWindowsFreshWrapperMustPrepareDespiteSameFileID(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := guard.Root()
	root.ancestryAuthority = windowsAncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	provider := &windowsCountingObjectIDProvider{identity: windowsPersistentObjectID{0x44}}
	root.objectIDs = provider
	root.objectIDState = newWindowsPersistentObjectIDState()
	const name = "fresh-selected-directory"
	if err := os.Mkdir(filepath.Join(root.path, name), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := root.OpenDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := first.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := root.OpenDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.identityClaim(); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("fresh wrapper inherited FileID-keyed authority: %v", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("unprepared fresh wrapper invoked CreateOrGet %d times", calls)
	}
	rebound, err := reopened.prepareIdentityClaim()
	if err != nil || !bytes.Equal(prepared, rebound) {
		t.Fatalf("fresh wrapper preparation equal=%t error=%v", bytes.Equal(prepared, rebound), err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("fresh wrapper preparation invoked CreateOrGet %d times", calls)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root.path, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.path, name), 0o700); err != nil {
		t.Fatal(err)
	}
	replacement, err := root.OpenDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err := replacement.identityClaim(); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("replacement FileID inherited cached claim: %v", err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("replacement read-only miss invoked CreateOrGet %d times", calls)
	}
}

func TestWindowsGuardedDirectoryProvenanceSurvivesEveryReopenLane(t *testing.T) {
	rootPath := windowsNativeTestTempDir(t)
	platform, err := Open(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close output platform: %v", err)
		}
	})
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close output ancestry guard: %v", err)
		}
	})
	root := guard.Root()
	assertWindowsDirectoryProvenance(t, "guard root", root, false)
	rootNative := root.(*windowsOutputDirectory).native
	rootClaim, err := rootNative.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}

	duplicate, err := root.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsDirectoryProvenance(t, "duplicate", duplicate, false)
	duplicateNative := duplicate.(*windowsOutputDirectory).native
	if duplicateNative.objectIDState != rootNative.objectIDState {
		t.Fatal("true duplicate did not share the prepared handle-local identity state")
	}
	if claim, claimErr := duplicateNative.identityClaim(); claimErr != nil || !bytes.Equal(claim, rootClaim) {
		t.Fatalf("duplicate identity equal=%t error=%v", bytes.Equal(claim, rootClaim), claimErr)
	}
	if err := duplicate.Close(); err != nil {
		t.Fatal(err)
	}

	const publicName = "selected-provenance"
	created, err := root.CreateDirectory(publicName, false)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsDirectoryProvenance(t, "created public directory", created, false)
	createdNative := created.(*windowsOutputDirectory).native
	createdClaim, err := createdNative.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := root.OpenDirectory(publicName, false)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsDirectoryProvenance(t, "reopened public directory", reopened, false)
	reopenedNative := reopened.(*windowsOutputDirectory).native
	if _, err := reopenedNative.identityClaim(); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("fresh public reopen inherited prepared identity state: %v", err)
	}
	reopenedClaim, err := reopenedNative.prepareIdentityClaim()
	if err != nil || !bytes.Equal(createdClaim, reopenedClaim) {
		t.Fatalf("reopened public identity equal=%t error=%v", bytes.Equal(createdClaim, reopenedClaim), err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	entry, err := root.OpenEntry(publicName)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := root.OpenPinnedDirectory(entry, false)
	if err != nil {
		_ = entry.Close()
		t.Fatal(err)
	}
	assertWindowsDirectoryProvenance(t, "pinned public reopen", pinned, false)
	pinnedNative := pinned.(*windowsOutputDirectory).native
	if _, err := pinnedNative.identityClaim(); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("pinned public reopen inherited prepared identity state: %v", err)
	}
	pinnedClaim, err := pinnedNative.prepareIdentityClaim()
	if err != nil || !bytes.Equal(createdClaim, pinnedClaim) {
		t.Fatalf("pinned public identity equal=%t error=%v", bytes.Equal(createdClaim, pinnedClaim), err)
	}
	if err := errors.Join(pinned.Close(), entry.Close()); err != nil {
		t.Fatal(err)
	}

	privateCandidate, err := root.CreateDirectory("private-provenance-candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsDirectoryProvenance(t, "created private directory", privateCandidate, true)
	installed, err := root.InstallDirectoryNoReplace(privateCandidate, "private-provenance-installed")
	if err != nil {
		_ = privateCandidate.Close()
		t.Fatal(err)
	}
	assertWindowsDirectoryProvenance(t, "installed private directory", installed, true)
	if err := errors.Join(installed.Close(), privateCandidate.Close()); err != nil {
		t.Fatal(err)
	}
}

func assertWindowsDirectoryProvenance(
	t *testing.T,
	label string,
	directory outputcap.Directory,
	wantPrivate bool,
) {
	t.Helper()
	wrapper, ok := directory.(*windowsOutputDirectory)
	if !ok || wrapper == nil || wrapper.native == nil {
		t.Fatalf("%s type = %T", label, directory)
	}
	native := wrapper.native
	if native.private != wantPrivate {
		t.Fatalf("%s private=%t, want %t", label, native.private, wantPrivate)
	}
	if wantPrivate {
		if native.placementGuard || native.selfPlacementGuard {
			t.Fatalf("%s private provenance unexpectedly carries public placement flags", label)
		}
		return
	}
	if !native.placementGuard || !native.selfPlacementGuard {
		t.Fatalf("%s public provenance placement=%t self=%t", label, native.placementGuard, native.selfPlacementGuard)
	}
}

func TestWindowsObjectIDClaimSurvivesReopenOnlyAfterPreparation(t *testing.T) {
	rootPath := windowsNativeTestTempDir(t)
	platform, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	first, err := guard.Root().prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(guard.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	reopened, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedGuard, err := reopened.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedGuard.Close()
	if _, err := reopenedGuard.Root().identityClaim(); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("reopened authority read an unprepared claim: %v", err)
	}
	recovered, err := reopenedGuard.Root().prepareIdentityClaim()
	if err != nil || !bytes.Equal(first, recovered) {
		t.Fatalf("reopened claim equal=%t error=%v", bytes.Equal(first, recovered), err)
	}
}

func TestWindowsGuardDetectsRootReplacementGapAndNewObjectID(t *testing.T) {
	base := windowsNativeTestTempDir(t)
	rootPath := filepath.Join(base, "output")
	retiredPath := filepath.Join(base, "retired-output")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	originalClaim, err := guard.Root().prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rootPath, retiredPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	trap := &windowsObjectIDMutationTrap{}
	platform.root.objectIDs = trap
	if replacementGuard, err := platform.acquirePublicOperationGuard(); err == nil || !errors.Is(err, errWindowsOutputUnsafe) {
		if replacementGuard != nil {
			_ = replacementGuard.Close()
		}
		t.Fatalf("primary authority accepted replacement root: %v", err)
	}
	if calls := trap.calls.Load(); calls != 0 {
		t.Fatalf("failed guard acquisition invoked CreateOrGet %d times", calls)
	}
	if entries, readErr := os.ReadDir(rootPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("failed rebind left WindShare state/content entries=%v error=%v", entries, readErr)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}

	replacement, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	replacementGuard, err := replacement.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer replacementGuard.Close()
	replacementClaim, err := replacementGuard.Root().prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(originalClaim, replacementClaim) {
		t.Fatal("replacement root reused the retired root Object ID claim")
	}
	if entries, readErr := os.ReadDir(rootPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("replacement identity preparation left visible state/content entries=%v error=%v", entries, readErr)
	}
}
