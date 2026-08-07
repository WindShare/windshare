package outputruntime

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

// selectionWave6Directory models retained-directory evidence, not a pathname.
// Each test configures the exact capability returned by a no-follow transition,
// which keeps close failures and replacement checks on the same object graph as
// the production selection walk.
type selectionWave6Directory struct {
	outputcap.Directory
	openDirs map[string]outputcap.Directory
	openErrs map[string]error

	duplicate    outputcap.Directory
	duplicateErr error
	duplicateSet bool

	classKind  outputcap.EntryKind
	classExact bool
	classErr   error
	classSet   bool

	same    bool
	sameErr error
	sameSet bool

	createErr   error
	metadataErr error
	syncErr     error
	closeErr    error
}

func (directory *selectionWave6Directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if err := directory.openErrs[name]; err != nil {
		return nil, err
	}
	if child, ok := directory.openDirs[name]; ok {
		return child, nil
	}
	if directory.Directory == nil {
		return nil, fs.ErrNotExist
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *selectionWave6Directory) Duplicate() (outputcap.Directory, error) {
	if directory.duplicateSet {
		return directory.duplicate, directory.duplicateErr
	}
	if directory.Directory == nil {
		return nil, directory.duplicateErr
	}
	return directory.Directory.Duplicate()
}

func (directory *selectionWave6Directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory.classSet {
		return directory.classKind, directory.classExact, directory.classErr
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *selectionWave6Directory) SameDirectory(other outputcap.Directory) (bool, error) {
	if directory.sameSet {
		return directory.same, directory.sameErr
	}
	if directory.Directory == nil {
		return false, directory.sameErr
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *selectionWave6Directory) ValidateCreateAuthority() error {
	return directory.createErr
}

func (directory *selectionWave6Directory) ValidateMetadataAuthority() error {
	return directory.metadataErr
}

func (directory *selectionWave6Directory) Sync() error {
	if directory.Directory == nil {
		return directory.syncErr
	}
	return errors.Join(directory.Directory.Sync(), directory.syncErr)
}

func (directory *selectionWave6Directory) Close() error {
	if directory.Directory == nil {
		return directory.closeErr
	}
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

func TestCoverageWave6SelectionAuthorityWalkEdges(t *testing.T) {
	t.Run("root-authorities-are-independent", func(t *testing.T) {
		createErr := errors.New("root create authority")
		metadataErr := errors.New("root metadata authority")
		root := &selectionWave6Directory{createErr: createErr, metadataErr: metadataErr}
		err := preflightOutputDirectoryAuthorities(root, "", true, true)
		if !errors.Is(err, createErr) || !errors.Is(err, metadataErr) {
			t.Fatalf("root authority error = %v", err)
		}
	})

	t.Run("missing-descendant-requires-create-authority", func(t *testing.T) {
		createErr := errors.New("last parent create authority")
		root := &selectionWave6Directory{createErr: createErr}
		err := preflightOutputDirectoryAuthorities(root, "missing/child", false, false)
		if !errors.Is(err, createErr) {
			t.Fatalf("missing descendant authority = %v", err)
		}
	})

	t.Run("complete-descendant-validates-both-authorities-and-close", func(t *testing.T) {
		createErr := errors.New("selected create authority")
		metadataErr := errors.New("selected metadata authority")
		closeErr := errors.New("selected close")
		selected := &selectionWave6Directory{createErr: createErr, metadataErr: metadataErr, closeErr: closeErr}
		root := &selectionWave6Directory{openDirs: map[string]outputcap.Directory{"selected": selected}}
		err := preflightOutputDirectoryAuthorities(root, "selected", true, true)
		if !errors.Is(err, createErr) || !errors.Is(err, metadataErr) || !errors.Is(err, closeErr) {
			t.Fatalf("complete descendant authority = %v", err)
		}
	})

	t.Run("invalid-components-close-retained-parent", func(t *testing.T) {
		closeErr := errors.New("retained parent close")
		retained := &selectionWave6Directory{closeErr: closeErr}
		root := &selectionWave6Directory{openDirs: map[string]outputcap.Directory{"valid": retained}}
		_, err := walkOutputDirectoryPath(root, "valid/../escape", false)
		if !errors.Is(err, outputfault.ErrPathEscape) || !errors.Is(err, closeErr) {
			t.Fatalf("invalid component walk = %v", err)
		}
	})

	t.Run("failed-open-closes-returned-child-and-parent", func(t *testing.T) {
		childCloseErr := errors.New("failed child close")
		parentCloseErr := errors.New("retained parent close")
		openErr := errors.New("open child")
		failedChild := &selectionWave6Directory{closeErr: childCloseErr}
		retained := &selectionWave6Directory{
			openDirs: map[string]outputcap.Directory{"child": failedChild},
			openErrs: map[string]error{"child": openErr}, closeErr: parentCloseErr,
		}
		// An adapter may return a handle together with a diagnostic. Model that
		// explicitly so the walk proves both capabilities are released.
		retained.openErrs = nil
		retained.openDirs = nil
		retained.Directory = &selectionOpenErrorDirectory{child: failedChild, err: openErr, closeErr: parentCloseErr}
		root := &selectionWave6Directory{openDirs: map[string]outputcap.Directory{"parent": retained}}
		_, err := walkOutputDirectoryPath(root, "parent/child", false)
		if !errors.Is(err, openErr) || !errors.Is(err, parentCloseErr) {
			t.Fatalf("failed child transition = %v", err)
		}
	})

	t.Run("create-path-authority-failure", func(t *testing.T) {
		createErr := errors.New("cannot create child")
		root := &selectionWave6Directory{createErr: createErr}
		_, err := walkOutputDirectoryPath(root, "new", true)
		if !errors.Is(err, createErr) {
			t.Fatalf("create path authority = %v", err)
		}
	})
}

type selectionOpenErrorDirectory struct {
	outputcap.Directory
	child    outputcap.Directory
	err      error
	closeErr error
}

func (directory *selectionOpenErrorDirectory) OpenDirectory(string, bool) (outputcap.Directory, error) {
	return directory.child, directory.err
}

func (directory *selectionOpenErrorDirectory) Close() error { return directory.closeErr }

func TestCoverageWave6ExactReopenEvidence(t *testing.T) {
	t.Run("wrong-type-is-unsafe", func(t *testing.T) {
		parent := &selectionWave6Directory{same: true, sameSet: true, classKind: outputcap.EntryRegularFile, classExact: true, classSet: true}
		root := &selectionWave6Directory{duplicate: parent, duplicateSet: true}
		err := exactReopenMaterializedOutputDirectory(root, "selected", &selectionWave6Directory{})
		if !errors.Is(err, errOutputAncestryUnsafe) || !errors.Is(err, errOutputAncestryMismatch) {
			t.Fatalf("wrong-type exact reopen = %v", err)
		}
	})

	t.Run("missing-reopen-is-unsafe", func(t *testing.T) {
		parent := &selectionWave6Directory{
			same: true, sameSet: true, classKind: outputcap.EntryDirectory, classExact: true, classSet: true,
			openErrs: map[string]error{"selected": fs.ErrNotExist},
		}
		root := &selectionWave6Directory{duplicate: parent, duplicateSet: true}
		err := exactReopenMaterializedOutputDirectory(root, "selected", &selectionWave6Directory{})
		if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, errOutputAncestryUnsafe) {
			t.Fatalf("missing exact reopen = %v", err)
		}
	})

	t.Run("replacement-and-comparison-failures", func(t *testing.T) {
		reopened := &selectionWave6Directory{}
		parent := &selectionWave6Directory{
			same: true, sameSet: true, classKind: outputcap.EntryDirectory, classExact: true, classSet: true,
			openDirs: map[string]outputcap.Directory{"selected": reopened},
		}
		root := &selectionWave6Directory{duplicate: parent, duplicateSet: true}
		retained := &selectionWave6Directory{same: false, sameSet: true}
		if err := exactReopenMaterializedOutputDirectory(root, "selected", retained); !errors.Is(err, errOutputAncestryMismatch) {
			t.Fatalf("replacement exact reopen = %v", err)
		}

		compareErr := errors.New("compare directories")
		retained = &selectionWave6Directory{sameErr: compareErr, sameSet: true}
		if err := exactReopenMaterializedOutputDirectory(root, "selected", retained); !errors.Is(err, compareErr) {
			t.Fatalf("comparison exact reopen = %v", err)
		}
	})

	t.Run("success-still-reports-capability-close-errors", func(t *testing.T) {
		parentCloseErr := errors.New("parent close")
		reopenedCloseErr := errors.New("reopened close")
		reopened := &selectionWave6Directory{closeErr: reopenedCloseErr}
		parent := &selectionWave6Directory{
			same: true, sameSet: true, closeErr: parentCloseErr,
			classKind: outputcap.EntryDirectory, classExact: true, classSet: true,
			openDirs: map[string]outputcap.Directory{"selected": reopened},
		}
		root := &selectionWave6Directory{duplicate: parent, duplicateSet: true}
		retained := &selectionWave6Directory{same: true, sameSet: true}
		err := exactReopenMaterializedOutputDirectory(root, "selected", retained)
		if !errors.Is(err, parentCloseErr) || !errors.Is(err, reopenedCloseErr) {
			t.Fatalf("exact reopen cleanup = %v", err)
		}
	})
}

func TestCoverageWave6MaterializedDirectoryEvidence(t *testing.T) {
	modified, err := catalog.NewModifiedTime(1, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	selected := transfer.OutputSelectionDirectory{Path: "selected", ModifiedTime: modified}

	newGraph := func(retained *selectionWave6Directory) *selectionWave6Directory {
		reopened := &selectionWave6Directory{}
		parent := &selectionWave6Directory{
			same: true, sameSet: true, classKind: outputcap.EntryDirectory, classExact: true, classSet: true,
			openDirs: map[string]outputcap.Directory{"selected": reopened},
		}
		return &selectionWave6Directory{
			openDirs:  map[string]outputcap.Directory{"selected": retained},
			duplicate: parent, duplicateSet: true,
		}
	}

	t.Run("metadata-authority", func(t *testing.T) {
		metadataErr := errors.New("selected metadata authority")
		retained := &selectionWave6Directory{metadataErr: metadataErr}
		err := materializeSelectedOutputDirectory(newGraph(retained), selected)
		if !errors.Is(err, metadataErr) {
			t.Fatalf("materialize metadata authority = %v", err)
		}
	})

	t.Run("sync-and-close-failures", func(t *testing.T) {
		syncErr := errors.New("selected sync")
		closeErr := errors.New("selected close")
		retained := &selectionWave6Directory{syncErr: syncErr, closeErr: closeErr}
		err := materializeSelectedOutputDirectory(newGraph(retained), selected)
		if !errors.Is(err, syncErr) || !errors.Is(err, closeErr) {
			t.Fatalf("materialize sync/close = %v", err)
		}
	})

	t.Run("exact-reopen-replacement", func(t *testing.T) {
		retained := &selectionWave6Directory{same: false, sameSet: true}
		err := materializeSelectedOutputDirectory(newGraph(retained), selected)
		if !errors.Is(err, errOutputAncestryMismatch) {
			t.Fatalf("materialize replacement = %v", err)
		}
	})
}
