//go:build windows

package osfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutputV3PublicTestDecoratorsPreserveDirectoryAuthority(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	tests := []struct {
		name      string
		decorate  func(outputV3Directory) outputV3Directory
		decorated func(outputV3Directory) bool
	}{
		{
			name: "batch admission",
			decorate: func(root outputV3Directory) outputV3Directory {
				return &windowsV3BatchCountingDirectory{
					outputV3Directory: root,
					counts:            &windowsV3BatchAdmissionCounts{},
				}
			},
			decorated: func(directory outputV3Directory) bool {
				_, ok := directory.(*windowsV3BatchCountingDirectory)
				return ok
			},
		},
		{
			name: "publication permission",
			decorate: func(root outputV3Directory) outputV3Directory {
				return &outputV3PublicationPermissionDirectory{
					outputV3Directory: root,
					gate:              &outputV3PublicationPermissionGate{},
				}
			},
			decorated: func(directory outputV3Directory) bool {
				_, ok := directory.(*outputV3PublicationPermissionDirectory)
				return ok
			},
		},
		{
			name: "operation hold",
			decorate: func(root outputV3Directory) outputV3Directory {
				return &windowsV3OperationHoldDirectory{
					outputV3Directory: root,
					gate:              newWindowsV3OperationHoldGate("never-held"),
				}
			},
			decorated: func(directory outputV3Directory) bool {
				_, ok := directory.(*windowsV3OperationHoldDirectory)
				return ok
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := filepath.Join(base, test.name)
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatal(err)
			}
			platform, err := openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := platform.Close(); err != nil {
					t.Errorf("close conformance platform: %v", err)
				}
			})
			assertOutputV3PublicTestDecoratorConformance(
				t,
				test.decorate(platform.Root()),
				test.decorated,
			)
		})
	}
}

func assertOutputV3PublicTestDecoratorConformance(
	t *testing.T,
	root outputV3Directory,
	isDecorated func(outputV3Directory) bool,
) {
	t.Helper()
	duplicate, err := root.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	requireOutputV3OwnedTestDirectory(t, duplicate, "duplicate")
	if !isDecorated(duplicate) {
		t.Fatalf("duplicate lost decorator authority: %T", duplicate)
	}
	if same, err := duplicate.SameDirectory(root); err != nil || !same {
		t.Fatalf("decorated duplicate comparison = (%t, %v)", same, err)
	}

	created, err := root.CreateDirectory("decorated-child", false)
	if err != nil {
		t.Fatal(err)
	}
	requireOutputV3OwnedTestDirectory(t, created, "created child")
	if !isDecorated(created) {
		t.Fatalf("created child lost decorator authority: %T", created)
	}
	reopened, err := root.OpenDirectory("decorated-child", false)
	if err != nil {
		t.Fatal(err)
	}
	requireOutputV3OwnedTestDirectory(t, reopened, "reopened child")
	if !isDecorated(reopened) {
		t.Fatalf("reopened child lost decorator authority: %T", reopened)
	}
	if same, err := created.SameDirectory(reopened); err != nil || !same {
		t.Fatalf("decorated child comparison = (%t, %v)", same, err)
	}
}

func requireOutputV3OwnedTestDirectory(
	t *testing.T,
	directory outputV3Directory,
	operation string,
) {
	t.Helper()
	if directory == nil {
		t.Fatalf("%s returned no directory", operation)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Errorf("close %s: %v", operation, err)
		}
	})
}
