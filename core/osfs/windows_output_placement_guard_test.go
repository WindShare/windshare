//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

const (
	windowsV3PlacementDirectory = "guarded"
	windowsV3PlacementFile      = windowsV3PlacementDirectory + "/file.bin"
)

func TestWindowsV3PublicDirectoryPlacementGuardPinsCompleteAncestry(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "root")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	parent, err := platform.root.openDirectory(windowsV3PlacementDirectory, false, windows.FILE_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	parentPath := filepath.Join(rootPath, windowsV3PlacementDirectory)
	displacedPath := filepath.Join(base, "displaced")
	if err := os.Rename(parentPath, displacedPath); !v3RecoveryIsBlockedAncestorReplacement(err) {
		t.Fatalf("move live public directory error = %v, want placement denial", err)
	}

	child, err := parent.openDirectory("child", false, windows.FILE_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parentPath, displacedPath); !v3RecoveryIsBlockedAncestorReplacement(err) {
		t.Fatalf("move ancestor of live public descendant error = %v, want placement denial", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parentPath, displacedPath); err != nil {
		t.Fatalf("move after placement authority closed: %v", err)
	}
}

func TestWindowsV3GuardedPublicationCannotFollowDisplacedParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(root)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	parent, err := platform.root.openDirectory(windowsV3PlacementDirectory, false, windows.FILE_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	source, err := platform.root.CreatePrivateFile("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}

	parentPath := filepath.Join(root, windowsV3PlacementDirectory)
	displaced := filepath.Join(base, "publication-displaced")
	if err := os.Rename(parentPath, displaced); !v3RecoveryIsBlockedAncestorReplacement(err) {
		t.Fatalf("move immediately before publication error = %v, want placement denial", err)
	}
	published, err := parent.LinkRegularFileNoReplace(source, "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := published.Close(); err != nil {
		t.Fatal(err)
	}
	windowsV3RequirePlacementResult(t, root, displaced, []byte("data"))
}

func TestWindowsV3ParentDisplacementCutsFailClosedAndRecoverInsideRoot(t *testing.T) {
	t.Run("before begin", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := windowsV3PlacementSelection(t, 4)
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
		parent := filepath.Join(root, windowsV3PlacementDirectory)
		displaced := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-before-begin-displaced")
		t.Cleanup(func() { _ = os.RemoveAll(displaced) })
		if err := os.Rename(parent, displaced); err != nil {
			t.Fatalf("move parent between admission and BeginFile: %v", err)
		}
		file := v3RecoveryOutputFile(t, opened.Session, selection, 4)
		if start, err := opened.Session.BeginFile(context.Background(), file); err == nil {
			t.Fatalf("BeginFile after parent displacement = (start=%+v, err=%v), want pre-record failure", start, err)
		}
		if names, err := opened.Session.filesDir.Names(1); err != nil || len(names) != 0 {
			t.Fatalf("file-state entries after rejected BeginFile = %v, err=%v", names, err)
		}
		if _, err := os.Stat(filepath.Join(displaced, "file.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outside final after rejected BeginFile: %v", err)
		}
		v3RecoveryCloseSession(t, opened.Session)
	})

	t.Run("after checkpoint and recovery publication", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := windowsV3PlacementSelection(t, 4)
		sessionIDs := &v3RecoverySessionIDs{}
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
		file := v3RecoveryOutputFile(t, opened.Session, selection, 4)
		transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Checkpoint(context.Background()); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(root, windowsV3PlacementDirectory)
		displaced := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-checkpoint-displaced")
		t.Cleanup(func() { _ = os.RemoveAll(displaced) })
		if err := os.Rename(parent, displaced); err != nil {
			t.Fatalf("move parent after checkpoint: %v", err)
		}
		if settlement, err := transaction.Commit(context.Background()); err == nil || settlement.Kind() != 0 {
			t.Fatalf("commit through displaced parent = (kind=%v, err=%v), want retained failure", settlement.Kind(), err)
		}
		if _, err := os.Stat(filepath.Join(displaced, "file.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outside final after failed publication: %v", err)
		}
		paused, err := opened.Session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		if err != nil || paused.Kind() != transfer.JobPaused {
			t.Fatalf("pause witnessed publication = (kind=%v, err=%v)", paused.Kind(), err)
		}
		if err := os.Rename(displaced, parent); err != nil {
			t.Fatalf("restore selected parent: %v", err)
		}

		reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
		recoveryFile := v3RecoveryOutputFile(t, reopened.Session, selection, 4)
		start, err := reopened.Session.BeginFile(context.Background(), recoveryFile)
		if err != nil {
			t.Fatal(err)
		}
		settlement, immediate := start.ImmediateSettlement()
		if !immediate || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("recovery publication = (kind=%v, immediate=%t), want published", settlement.Kind(), immediate)
		}
		windowsV3RequirePlacementResult(t, root, displaced, []byte("data"))
		v3RecoveryCloseSession(t, reopened.Session)
	})
}

func windowsV3PlacementSelection(t *testing.T, size uint64) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](1)
	root := v3RecoveryIdentity16[catalog.DirectoryID](2)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](3)
	parent := v3RecoveryIdentity16[catalog.DirectoryID](4)
	parentGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](5)
	modified := v3RecoveryModifiedTime(t)
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]transfer.OutputSelectionDirectory{{
			Path: windowsV3PlacementDirectory, DirectoryID: parent,
			Generation: parentGeneration, ModifiedTime: modified,
		}},
		[]transfer.OutputSelectionFile{{
			Path: windowsV3PlacementFile, FileID: v3RecoveryIdentity16[catalog.FileID](6),
			ParentDirectoryID: parent, ParentGeneration: parentGeneration,
			ExpectedSize: size, ModifiedTime: modified,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func windowsV3RequirePlacementResult(t *testing.T, root, displaced string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(root, windowsV3PlacementFile))
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("published bytes = %q, err=%v, want %q", actual, err, expected)
	}
	if _, err := os.Stat(displaced); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("displaced tree exists after guarded publication: %v", err)
	}
}
