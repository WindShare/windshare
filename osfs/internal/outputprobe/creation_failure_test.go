package outputprobe

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestProbeFailedCreateWithoutWitnessPreservesUnownedEntry(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	directory := workspace.directory.(*probeTestDirectory)
	workspace.directory = &publicationTestDirectory{probeTestDirectory: directory, mode: "success"}
	root.system.fault = "created-file:" + StageName
	if err := workspace.FilePublication(); !errors.Is(err, errInjected) {
		t.Fatalf("creation error=%v", err)
	}
	workspace.directory = directory
	if err := workspace.Cleanup(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unwitnessed creation cleanup=%v", err)
	}
	if directory.object.files[StageName] == nil {
		t.Fatal("live cleanup deleted an entry without a creation witness")
	}
	assertProbeHandlesReleased(t, root)
}

func TestProbeRejectedCreateDoesNotPreventCleanup(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	directory := workspace.directory.(*probeTestDirectory)
	workspace.directory = &publicationTestDirectory{probeTestDirectory: directory, mode: "success"}
	root.system.fault = "create-file:" + StageName
	if err := workspace.FilePublication(); !errors.Is(err, errInjected) {
		t.Fatalf("creation error=%v", err)
	}
	workspace.directory = directory
	if err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(root.object.directories) != 0 {
		t.Fatal("failed create without a namespace mutation left the workspace")
	}
	assertProbeHandlesReleased(t, root)
}
