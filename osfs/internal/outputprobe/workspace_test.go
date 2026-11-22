package outputprobe

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"slices"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func newProbeTestWorkspace(t *testing.T, root *probeTestDirectory, index int) *Workspace {
	t.Helper()
	name := probeTestName(index)
	directory, err := root.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	return NewWorkspace(root, name, directory)
}

func assertProbeHandlesReleased(t *testing.T, root *probeTestDirectory) {
	t.Helper()
	if root.system.openHandles != 1 || root.closed {
		t.Fatalf("open handles=%d, borrowed root closed=%t", root.system.openHandles, root.closed)
	}
}

func TestProbeCleanupRetainsWitnessAcrossPartialDirectoryInstall(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	root.system.fault = "installed-directory:installed"
	if err := workspace.OperationRecovery(); !errors.Is(err, errInjected) {
		t.Fatalf("proof error=%v", err)
	}
	if err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(root.object.directories) != 0 {
		t.Fatal("partially committed artifact remains")
	}
	assertProbeHandlesReleased(t, root)
}

func TestProbeCleanupStopsAtForeignReplacement(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	directory := workspace.Directory().(*probeTestDirectory)
	workspace.directory = &publicationTestDirectory{probeTestDirectory: directory, mode: "success"}
	if err := workspace.FilePublication(); err != nil {
		t.Fatal(err)
	}
	workspace.directory = directory
	foreign := &probeTestFileObject{size: 1}
	directory.object.files[PublicationName] = foreign
	root.system.events = nil
	if err := workspace.Cleanup(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replacement cleanup error=%v", err)
	}
	if directory.object.files[PublicationName] != foreign {
		t.Fatal("cleanup crossed an unverified identity")
	}
	if slices.Contains(root.system.events, "remove-file:anchor") {
		t.Fatal("cleanup continued after its first unverified cut")
	}
	assertProbeHandlesReleased(t, root)
}

func TestProbeCleanupStopsAtFailedMutation(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	directory := workspace.Directory().(*probeTestDirectory)
	workspace.directory = &publicationTestDirectory{probeTestDirectory: directory, mode: "success"}
	if err := workspace.FilePublication(); err != nil {
		t.Fatal(err)
	}
	workspace.directory = directory
	root.system.fault = "remove-file:publication"
	if err := workspace.Cleanup(); !errors.Is(err, errInjected) {
		t.Fatalf("cleanup error=%v", err)
	}
	if len(directory.object.files) != 2 {
		t.Fatal("later cleanup entries were removed")
	}
	assertProbeHandlesReleased(t, root)
}

func TestProbeAllocationNeverAdoptsCollision(t *testing.T) {
	root := newProbeTestRoot()
	nonce := bytes.Repeat([]byte{1}, RandomBytes)
	name, err := NewName(bytes.NewReader(nonce))
	if err != nil {
		t.Fatal(err)
	}
	existing := newProbeTestDirectoryObject()
	existing.files["foreign"] = &probeTestFileObject{size: 23}
	root.object.directories[name] = existing
	entropy := append(nonce, bytes.Repeat([]byte{2}, RandomBytes)...)
	workspace, err := Allocate(root, bytes.NewReader(entropy))
	if err != nil {
		t.Fatal(err)
	}
	if workspace.name == name {
		t.Fatal("existing namespace adopted")
	}
	if err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if root.object.directories[name] != existing {
		t.Fatal("collision namespace changed")
	}
	if _, err := Allocate(root, bytes.NewReader(nonce)); !errors.Is(err, io.EOF) {
		t.Fatalf("exhausted allocation entropy=%v", err)
	}
	assertProbeHandlesReleased(t, root)
}

func TestWorkspaceCloseNeverDeletesInspectedObjects(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	if err := workspace.OperationRecovery(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if len(root.object.directories[probeTestName(1)].directories) != 2 {
		t.Fatal("close mutated namespace")
	}
	if err := (*Workspace)(nil).Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := (&Workspace{}).Cleanup(); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("uninitialized workspace cleanup error=%v", err)
	}
	if err := (*Workspace)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	assertProbeHandlesReleased(t, root)
}
