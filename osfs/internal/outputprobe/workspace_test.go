package outputprobe

import (
	"bytes"
	"errors"
	"io"
	"reflect"
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

func TestSharedProbeExercisesBothNativeLayouts(t *testing.T) {
	for _, profile := range []Profile{LinuxExt4, WindowsNTFS} {
		t.Run(probeTestName(int(profile)), func(t *testing.T) {
			root := newProbeTestRoot()
			workspace := newProbeTestWorkspace(t, root, 1)
			for _, proof := range []func() error{
				func() error { return workspace.SafePublish(profile) },
				workspace.RangeRecovery, workspace.OperationRecovery, workspace.VerifyFiles,
			} {
				if err := proof(); err != nil {
					t.Fatal(err)
				}
			}
			inspected, err := Inspect(root, workspace.name, profile)
			if err != nil {
				t.Fatal(err)
			}
			if err := inspected.Close(); err != nil {
				t.Fatal(err)
			}
			if err := workspace.Cleanup(); err != nil {
				t.Fatal(err)
			}
			if len(root.object.directories) != 0 {
				t.Fatal("probe namespace remains")
			}
			assertProbeHandlesReleased(t, root)
		})
	}
}

func TestProbeCleanupRetainsWitnessAcrossPartialMutations(t *testing.T) {
	for _, fault := range []string{
		"linked-file:anchor", "linked-file:publication",
		"replaced-file:record", "installed-directory:installed",
	} {
		t.Run(fault, func(t *testing.T) {
			root := newProbeTestRoot()
			workspace := newProbeTestWorkspace(t, root, 1)
			root.system.fault = fault
			var err error
			switch fault {
			case "replaced-file:record":
				err = workspace.RangeRecovery()
			case "installed-directory:installed":
				err = workspace.OperationRecovery()
			default:
				err = workspace.SafePublish(WindowsNTFS)
			}
			if !errors.Is(err, errInjected) {
				t.Fatalf("proof error=%v", err)
			}
			if err := workspace.Cleanup(); err != nil {
				t.Fatal(err)
			}
			if len(root.object.directories) != 0 {
				t.Fatal("partially committed artifact remains")
			}
			assertProbeHandlesReleased(t, root)
		})
	}
}

func TestProbeCleanupStopsAtForeignReplacement(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	if err := workspace.SafePublish(WindowsNTFS); err != nil {
		t.Fatal(err)
	}
	directory := workspace.Directory().(*probeTestDirectory)
	foreign := &probeTestFileObject{size: 1}
	directory.object.files[StageName] = foreign
	root.system.events = nil
	if err := workspace.Cleanup(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replacement cleanup error=%v", err)
	}
	if len(directory.object.files) != 3 || directory.object.files[StageName] != foreign {
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
	if err := workspace.SafePublish(WindowsNTFS); err != nil {
		t.Fatal(err)
	}
	root.system.fault = "remove-file:stage"
	if err := workspace.Cleanup(); !errors.Is(err, errInjected) {
		t.Fatalf("cleanup error=%v", err)
	}
	directory := root.object.directories[probeTestName(1)]
	if len(directory.files) != 3 {
		t.Fatal("later cleanup entries were removed")
	}
	assertProbeHandlesReleased(t, root)
	if err := Recover(root, probeTestNames(root), WindowsNTFS); err != nil {
		t.Fatal(err)
	}
	assertProbeHandlesReleased(t, root)
}

func TestRangeProbeKeepsOldWitnessUntilNewRecordIsDurable(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	root.system.fault = "sync-file:record.tmp"
	if err := workspace.RangeRecovery(); !errors.Is(err, errInjected) {
		t.Fatalf("range probe error=%v", err)
	}
	if err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
	assertProbeHandlesReleased(t, root)
}

func TestRangeProbeCleansOriginalAfterReplacementRejected(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	root.system.fault = "replace-file:record"
	if err := workspace.RangeRecovery(); !errors.Is(err, errInjected) {
		t.Fatalf("range probe error=%v", err)
	}
	if err := workspace.Cleanup(); err != nil {
		t.Fatalf("failed replacement must retain cleanup authority for both generations: %v", err)
	}
	if len(root.object.directories) != 0 {
		t.Fatal("rejected replacement left a probe namespace")
	}
	assertProbeHandlesReleased(t, root)
}

func TestRecoveryValidatesEveryLeftoverBeforeDeletion(t *testing.T) {
	root := newProbeTestRoot()
	first := newProbeTestWorkspace(t, root, 1)
	if err := first.SafePublish(WindowsNTFS); err != nil {
		t.Fatal(err)
	}
	second := newProbeTestWorkspace(t, root, 2)
	second.Directory().(*probeTestDirectory).object.files["foreign"] = &probeTestFileObject{size: 23}
	if err := errors.Join(first.Close(), second.Close()); err != nil {
		t.Fatal(err)
	}
	before := probeTestNames(root)
	if err := Recover(root, before, WindowsNTFS); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("foreign leftover error=%v", err)
	}
	if !reflect.DeepEqual(probeTestNames(root), before) || len(root.object.directories[before[0]].files) != 3 {
		t.Fatal("earlier valid leftover was changed before all inspections completed")
	}
	assertProbeHandlesReleased(t, root)
}

func TestRecoveryBoundsAndNamesRejectWithoutMutation(t *testing.T) {
	for _, names := range [][]string{
		{ReservedPrefix}, {NamePrefix + "not-canonical"}, make([]string, MaximumLeftovers+1),
	} {
		root := newProbeTestRoot()
		if err := Recover(root, names, LinuxExt4); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("recovery error=%v", err)
		}
		if len(root.system.events) != 0 {
			t.Fatal("invalid recovery input touched the namespace")
		}
		assertProbeHandlesReleased(t, root)
	}
}

func TestRecoveryRejectsDifferentObjectsAtDataLinkNames(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	directory := workspace.Directory().(*probeTestDirectory)
	directory.object.files[StageName] = &probeTestFileObject{size: 1}
	directory.object.files[AnchorName] = &probeTestFileObject{size: 1}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root, probeTestNames(root), WindowsNTFS); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("mismatched link error=%v", err)
	}
	if len(directory.object.files) != 2 {
		t.Fatal("unowned files were removed")
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
	if err := workspace.SafePublish(WindowsNTFS); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if len(root.object.directories[probeTestName(1)].files) != 3 {
		t.Fatal("close mutated namespace")
	}
	if err := (*Workspace)(nil).Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := (&Workspace{}).Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := (*Workspace)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	assertProbeHandlesReleased(t, root)
}
