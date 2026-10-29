package outputprobe

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestFilePublicationProvesCommitAndCollisionAndCleansAmbiguousResults(t *testing.T) {
	for _, mode := range []string{"success", "unsupported", "before mutation", "after mutation", "overwrite", "false commit"} {
		t.Run(mode, func(t *testing.T) {
			root := newProbeTestRoot()
			workspace := newProbeTestWorkspace(t, root, 1)
			directory := workspace.directory.(*probeTestDirectory)
			workspace.directory = &publicationTestDirectory{probeTestDirectory: directory, mode: mode}
			err := workspace.FilePublication()
			workspace.directory = directory
			switch mode {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
			case "unsupported":
				if !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
					t.Fatalf("unsupported = %v", err)
				}
			case "before mutation", "after mutation":
				if !errors.Is(err, errInjected) {
					t.Fatalf("injected = %v", err)
				}
			case "overwrite":
				if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
					t.Fatalf("overwrite accepted: %v", err)
				}
			case "false commit":
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("false commit accepted: %v", err)
				}
			}
			// Cleanup still owns every possible image after an uncertain publish,
			// including a provider that replaced our collision fixture.
			if err := workspace.Cleanup(); err != nil {
				t.Fatal(err)
			}
			if len(root.object.directories) != 0 {
				t.Fatal("probe artifacts remain")
			}
			assertProbeHandlesReleased(t, root)
		})
	}
}

type publicationTestDirectory struct {
	*probeTestDirectory
	mode string
}

func (*publicationTestDirectory) LinkFileNoReplace(outputcap.FileIdentity, string) (outputcap.ObservedFile, error) {
	panic("file publication proof must not use hard links")
}
func (directory *publicationTestDirectory) PublishFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.PublishNoReplaceOutcome, error) {
	switch directory.mode {
	case "unsupported":
		return 0, outputcap.ErrOrdinaryOutputUnsupported
	case "before mutation":
		return outputcap.PublishNoReplaceIndeterminate, errInjected
	case "false commit":
		return outputcap.PublishNoReplaceCommitted, nil
	}
	_, exists := directory.object.files[name]
	if exists && directory.mode != "overwrite" {
		return outputcap.PublishNoReplaceCollision, nil
	}
	file := source.(*probeTestFile)
	directory.object.files[name] = file.object
	delete(directory.object.files, file.name)
	if directory.mode == "after mutation" {
		return outputcap.PublishNoReplaceIndeterminate, errInjected
	}
	if exists {
		return outputcap.PublishNoReplaceCollision, nil
	}
	return outputcap.PublishNoReplaceCommitted, nil
}
