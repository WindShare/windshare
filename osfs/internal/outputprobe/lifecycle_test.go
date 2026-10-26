package outputprobe

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
)

func TestClosedWorkspaceRejectsOperationsUntilRecovery(t *testing.T) {
	for _, test := range []struct {
		name         string
		populated    bool
		cleanup      bool
		fault        string
		wantLeftover bool
	}{
		{name: "close-empty", wantLeftover: true},
		{name: "close-populated", populated: true, wantLeftover: true},
		{name: "cleanup-success", populated: true, cleanup: true},
		{name: "cleanup-file-failure", populated: true, cleanup: true, fault: "remove-file:stage", wantLeftover: true},
		{name: "cleanup-directory-failure", cleanup: true, fault: "remove-directory:" + probeTestName(1), wantLeftover: true},
		{name: "cleanup-sync-failure", cleanup: true, fault: "sync-directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newProbeTestRoot()
			workspace := newProbeTestWorkspace(t, root, 1)
			if test.populated {
				if err := workspace.SafePublish(WindowsNTFS); err != nil {
					t.Fatal(err)
				}
			}
			root.system.fault = test.fault
			var err error
			if test.cleanup {
				err = workspace.Cleanup()
			} else {
				err = workspace.Close()
			}
			if test.fault != "" {
				if !errors.Is(err, errInjected) {
					t.Fatalf("first cleanup error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			assertProbeHandlesReleased(t, root)

			before := slices.Clone(root.system.events)
			for _, operation := range []struct {
				name string
				run  func() error
			}{
				{"clear", workspace.Clear},
				{"cleanup", workspace.Cleanup},
				{"safe-publish", func() error { return workspace.SafePublish(WindowsNTFS) }},
				{"range-recovery", workspace.RangeRecovery},
				{"operation-recovery", workspace.OperationRecovery},
				{"verify-files", workspace.VerifyFiles},
				{"verify-file", func() error { return workspace.VerifyFile(StageName, true) }},
				{"keep-nil-file", func() error { return workspace.KeepFile(StageName, nil) }},
			} {
				t.Run(operation.name, func(t *testing.T) {
					if err := operation.run(); !errors.Is(err, fs.ErrClosed) {
						t.Fatalf("operation after close error=%v", err)
					}
				})
			}
			if err := workspace.Close(); err != nil {
				t.Fatalf("repeated close error=%v", err)
			}
			if !slices.Equal(root.system.events, before) {
				t.Fatal("closed workspace attempted another filesystem operation")
			}
			leftover, exists := root.object.directories[workspace.name]
			if exists != test.wantLeftover {
				t.Fatalf("leftover exists=%t, want %t", exists, test.wantLeftover)
			}
			if exists && test.populated && len(leftover.files) != 3 {
				t.Fatal("closed workspace changed retained artifacts")
			}
			assertProbeHandlesReleased(t, root)

			if err := Recover(root, probeTestNames(root), WindowsNTFS); err != nil {
				t.Fatalf("fresh inspection could not recover leftovers: %v", err)
			}
			if len(root.object.directories) != 0 {
				t.Fatal("recovery left probe artifacts")
			}
			assertProbeHandlesReleased(t, root)
		})
	}
}

func TestClearRetainsLiveWitnessesForRetryAndReuse(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	if err := workspace.SafePublish(LinuxExt4); err != nil {
		t.Fatal(err)
	}
	root.system.fault = "remove-file:stage"
	if err := workspace.Clear(); !errors.Is(err, errInjected) {
		t.Fatalf("first clear error=%v", err)
	}
	if err := workspace.Clear(); err != nil {
		t.Fatalf("clear with live witnesses could not retry: %v", err)
	}
	if err := workspace.Clear(); err != nil {
		t.Fatalf("empty live workspace could not be cleared: %v", err)
	}
	if err := workspace.SafePublish(LinuxExt4); err != nil {
		t.Fatalf("cleared workspace could not reuse probe names: %v", err)
	}
	if err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(root.object.directories) != 0 {
		t.Fatal("reused workspace left probe artifacts")
	}
	assertProbeHandlesReleased(t, root)
}

func TestKeepFileAfterCloseReleasesTransferredWitness(t *testing.T) {
	root := newProbeTestRoot()
	workspace := newProbeTestWorkspace(t, root, 1)
	file, err := workspace.Directory().CreateFile(StageName, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.KeepFile(StageName, file); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("late witness transfer error=%v", err)
	}
	if len(root.object.directories[workspace.name].files) != 1 {
		t.Fatal("releasing a late witness deleted its file")
	}
	assertProbeHandlesReleased(t, root)
}
