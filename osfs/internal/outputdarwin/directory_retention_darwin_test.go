//go:build darwin

package outputdarwin

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestDirectoryAndFileRetentionIsIndependentOfDepth(t *testing.T) {
	const maximumRetainedDescriptors = 2
	for _, depth := range []int{0, 3, 24} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			platform := openAuthorityFixture(t, t.TempDir())
			leaf := retainedTestDirectory(t, platform.root, depth)
			source, err := leaf.CreateFile("source", false, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			before := openDescriptorCount(t)
			copy, err := leaf.Duplicate()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = copy.Close() })
			directoryCost := openDescriptorCount(t) - before
			if directoryCost > maximumRetainedDescriptors {
				t.Fatalf("directory copy retains %d descriptors at depth %d", directoryCost, depth)
			}
			if err := copy.Close(); err != nil {
				t.Fatal(err)
			}
			file, err := leaf.OpenObservedFile("source", false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			fileCost := openDescriptorCount(t) - before
			if fileCost > maximumRetainedDescriptors {
				t.Fatalf("open file retains %d descriptors at depth %d", fileCost, depth)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if after := openDescriptorCount(t); after != before {
				t.Fatalf("closed copies changed descriptor count: before=%d after=%d", before, after)
			}
			t.Logf("depth=%d directory-copy-fds=%d open-file-fds=%d", depth, directoryCost, fileCost)
		})
	}
}

func TestRetainedOriginsSurviveClosingTheirSourceDirectories(t *testing.T) {
	platform := openAuthorityFixture(t, t.TempDir())
	leaf := retainedTestDirectory(t, platform.root, 8)
	candidate, err := leaf.CreateDirectory("candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	copy, err := candidate.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	source, err := leaf.CreateFile("source", false, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(candidate.Close(), leaf.Close()); err != nil {
		t.Fatal(err)
	}
	installed, err := platform.root.InstallDirectoryNoReplace(copy, "installed")
	if err != nil {
		t.Fatal(err)
	}
	defer installed.Close()
	if same, err := installed.SameDirectory(copy); err != nil || !same {
		t.Fatalf("installed directory lost its original identity: same=%t err=%v", same, err)
	}
	outcome, err := platform.root.PublishFileNoReplace(source, "published")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publication from retained parent: outcome=%v err=%v", outcome, err)
	}
	final, err := platform.root.OpenObservedFile("published", false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if same, err := final.SameFile(source); err != nil || !same {
		t.Fatalf("publication lost its original file: same=%t err=%v", same, err)
	}
}

func TestDirectoryCopiesHaveIndependentEnumerationCursors(t *testing.T) {
	platform := openAuthorityFixture(t, t.TempDir())
	for _, name := range []string{"first", "second", "third"} {
		child, err := platform.root.CreateDirectory(name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := platform.root.file.Readdirnames(1); err != nil {
		t.Fatal(err)
	}
	first, err := platform.root.ReadNameBatch(1)
	if err != nil || len(first.Names) != 1 || !first.HasMore {
		t.Fatalf("first batch=%+v err=%v", first, err)
	}
	second, err := platform.root.ReadNameBatch(1)
	if err != nil || len(second.Names) != 1 || !second.HasMore || first.Names[0] != second.Names[0] {
		t.Fatalf("independent batch=%+v first=%+v err=%v", second, first, err)
	}
}

// /dev/fd measures the actual native ownership cost, including hidden origins.
// These tests stay serial so unrelated fixture activity cannot change the count.
func openDescriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func retainedTestDirectory(t *testing.T, root *directory, depth int) *directory {
	t.Helper()
	current := root
	for range depth {
		child, err := current.CreateDirectory("nested", false)
		if err != nil {
			t.Fatal(err)
		}
		if current != root {
			if err := current.Close(); err != nil {
				_ = child.Close()
				t.Fatal(err)
			}
		}
		current = child.(*directory)
	}
	t.Cleanup(func() { _ = current.Close() })
	return current
}
