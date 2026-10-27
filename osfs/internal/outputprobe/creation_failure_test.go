package outputprobe

import (
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestProbeFailedCreateWithoutWitnessPreservesRecoveryCut(t *testing.T) {
	for _, profile := range []Profile{EmptyDataLinks, SizedDataLinks} {
		for _, name := range []string{StageName, RecordName, TemporaryRecordName} {
			t.Run(fmt.Sprintf("%d/%s", profile, name), func(t *testing.T) {
				root := newProbeTestRoot()
				workspace := newProbeTestWorkspace(t, root, 1)
				if name != StageName {
					if err := workspace.SafePublish(profile); err != nil {
						t.Fatal(err)
					}
				}
				root.system.fault = "created-file:" + name
				var proofErr error
				if name == StageName {
					proofErr = workspace.SafePublish(profile)
				} else {
					proofErr = workspace.RangeRecovery()
				}
				if !errors.Is(proofErr, errInjected) {
					t.Fatalf("creation error=%v", proofErr)
				}
				if err := workspace.Cleanup(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
					t.Fatalf("unwitnessed creation cleanup=%v", err)
				}
				directory := root.object.directories[workspace.name]
				if directory.files[name] == nil {
					t.Fatal("live cleanup deleted an entry without a creation witness")
				}
				if name == TemporaryRecordName && directory.files[RecordName] == nil {
					t.Fatal("cleanup orphaned the temporary record")
				}
				assertProbeHandlesReleased(t, root)
				if err := Recover(root, probeTestNames(root), profile); err != nil {
					t.Fatalf("failed creation left an unreachable restart cut: %v", err)
				}
				if len(root.object.directories) != 0 {
					t.Fatal("restart left a probe namespace")
				}
				assertProbeHandlesReleased(t, root)
			})
		}
	}
}

func TestProbeRejectedCreateDoesNotPreventCleanup(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	root.system.fault = "create-file:" + StageName
	if err := workspace.SafePublish(SizedDataLinks); !errors.Is(err, errInjected) {
		t.Fatalf("creation error=%v", err)
	}
	if err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(root.object.directories) != 0 {
		t.Fatal("failed create without a namespace mutation left the workspace")
	}
	assertProbeHandlesReleased(t, root)
}
