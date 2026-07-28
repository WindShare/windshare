//go:build windows

package osfs

import (
	"context"
	"testing"
)

func TestOutputV3ListCleanNTFSRootLeavesNoResumeState(t *testing.T) {
	rootPath := t.TempDir()
	before, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Skipf("certified NTFS output root unavailable: %v", err)
	}
	if names, err := before.Root().Names(1); err != nil || len(names) != 0 {
		_ = before.Close()
		t.Fatalf("clean NTFS root entries before list = %v, %v", names, err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	inventory, err := ListResumeState(context.Background(), FilesystemResumeRoot{RootPath: rootPath})
	if err != nil {
		t.Fatalf("list clean NTFS root: %v", err)
	}
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("clean NTFS root resume summaries = %+v", summaries)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if names, err := after.Root().Names(1); err != nil || len(names) != 0 {
		t.Fatalf("clean NTFS root entries after list = %v, %v", names, err)
	}
}
