//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	windowsPlacementDirectory = "guarded"
	windowsPlacementFile      = windowsPlacementDirectory + "/file.bin"
)

func TestWindowsPublicDirectoryPlacementGuardPinsCompleteAncestry(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "root")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	parent, err := platform.root.openDirectory(windowsPlacementDirectory, false, windows.FILE_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	parentPath := filepath.Join(rootPath, windowsPlacementDirectory)
	displacedPath := filepath.Join(base, "displaced")
	if err := os.Rename(parentPath, displacedPath); !windowsIsBlockedAncestorReplacement(err) {
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
	if err := os.Rename(parentPath, displacedPath); !windowsIsBlockedAncestorReplacement(err) {
		t.Fatalf("move ancestor of live public descendant error = %v, want placement denial", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parentPath, displacedPath); err != nil {
		t.Fatalf("move after placement authority closed: %v", err)
	}
}

func TestWindowsGuardedPublicationCannotFollowDisplacedParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(root)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	parent, err := platform.root.openDirectory(windowsPlacementDirectory, false, windows.FILE_CREATE)
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

	parentPath := filepath.Join(root, windowsPlacementDirectory)
	displaced := filepath.Join(base, "publication-displaced")
	if err := os.Rename(parentPath, displaced); !windowsIsBlockedAncestorReplacement(err) {
		t.Fatalf("move immediately before publication error = %v, want placement denial", err)
	}
	published, err := parent.LinkRegularFileNoReplace(source, "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := published.Close(); err != nil {
		t.Fatal(err)
	}
	windowsRequirePlacementResult(t, root, displaced, []byte("data"))
}

func windowsRequirePlacementResult(t *testing.T, root, displaced string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(root, windowsPlacementFile))
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("published bytes = %q, err=%v, want %q", actual, err, expected)
	}
	if _, err := os.Stat(displaced); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("displaced tree exists after guarded publication: %v", err)
	}
}
