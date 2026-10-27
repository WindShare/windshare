//go:build darwin

package outputruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputdarwin"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestNativeCrashInventoryDiagnostic(t *testing.T) {
	root := os.Getenv("WINDSHARE_NATIVE_CRASH_DIAGNOSTIC_ROOT")
	if root == "" {
		t.Skip("diagnostic fixture absent")
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			t.Logf("entry %s size=%d mode=%v", path, info.Size(), info.Mode())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	repo, err := NewNativeResumeRepository(root, func(path string, create bool) (outputcap.Platform, error) {
		return outputdarwin.Open(path, create)
	}, FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) { t.Logf("trace %+v", event) }))
	if err != nil {
		t.Fatal(err)
	}
	page, err := repo.Page(context.Background(), resumeauthority.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range page.Headers() {
		value, err := repo.Acquire(context.Background(), header.Record().OperationID())
		if err != nil {
			t.Fatal(err)
		}
		lease := value.(*NativeResumeLease)
		t.Logf("top-level: %v", lease.ensureTopLevelLocked())
		present, err := lease.ensureFileStoreLocked()
		t.Logf("file store: present=%t error=%+v", present, err)
		if lease.store != nil {
			slots, attention := lease.store.LineageSnapshot()
			t.Logf("lineages=%+v attention=%+v", slots, attention)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
