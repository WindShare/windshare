//go:build windows

package outputwindows

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestWindowsProcessStageRetainsNativeObjectWithoutCleanupTicket(t *testing.T) {
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := &windowsOutputV3Directory{native: guard.Root()}
	directory, err := root.CreateDirectory("process-stages", true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	stage, err := root.CreateProcessStage(directory, "partial", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := stage.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	outcome, err := root.PublishFileNoReplace(stage, "process.bin")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish=%v %v", outcome, err)
	}
	final, err := root.OpenObservedFile("process.bin", false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	same, err := stage.SameFile(final)
	if err != nil || !same {
		t.Fatalf("same native object=%v %v", same, err)
	}
	if err := directory.RemoveFile("partial", stage); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("process-stages", directory); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if _, err := final.ReadAt(data, 0); err != nil || string(data) != "data" {
		t.Fatalf("final=%q %v", data, err)
	}
}
