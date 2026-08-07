//go:build windows

package osfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestOutputV3PublicTestDecoratorsPreserveDirectoryAuthority(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name      string
		decorate  func(outputcap.Directory) outputcap.Directory
		decorated func(outputcap.Directory) bool
	}{
		{
			name: "native conformance",
			decorate: func(root outputcap.Directory) outputcap.Directory {
				return &outputV3ConformanceDirectory{
					Directory: root,
				}
			},
			decorated: func(directory outputcap.Directory) bool {
				_, ok := directory.(*outputV3ConformanceDirectory)
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
			platform, err := openNativeOutputPlatform(rootPath, false)
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

// outputV3ConformanceDirectory proves the capability contract at the root
// native boundary without importing runtime-only fault fixtures into osfs.
type outputV3ConformanceDirectory struct {
	outputcap.Directory
}

func (directory *outputV3ConformanceDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &outputV3ConformanceDirectory{Directory: duplicate}, nil
}

func (directory *outputV3ConformanceDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3ConformanceDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *outputV3ConformanceDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3ConformanceDirectory{Directory: opened}, nil
}

func (directory *outputV3ConformanceDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3ConformanceDirectory{Directory: created}, nil
}

func assertOutputV3PublicTestDecoratorConformance(
	t *testing.T,
	root outputcap.Directory,
	isDecorated func(outputcap.Directory) bool,
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
	directory outputcap.Directory,
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
