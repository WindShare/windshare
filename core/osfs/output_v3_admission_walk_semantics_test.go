package osfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3AdmissionWalkStopsAtMissingDescendantsAndCreatesOnlyWhenAuthorized(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	if err := os.MkdirAll(filepath.Join(rootPath, "parent", "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, root := outputV3AdmissionWalkRoot(t, rootPath)

	if err := preflightOutputDirectoryPath(root, "parent/missing/child"); err != nil {
		t.Fatalf("read-only preflight of a missing suffix = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only preflight created a directory: %v", err)
	}
	if opened, err := openOutputDirectoryPath(root, "parent/missing/child", false); opened != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open missing suffix = (directory=%v, err=%v)", opened, err)
	}
	created, err := openOutputDirectoryPath(root, "parent/missing/child", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "parent", "missing", "child")); err != nil || !info.IsDir() {
		t.Fatalf("authorized create result = (%v, %v)", info, err)
	}

	for _, path := range []string{"", "parent//child", "parent/./child", "parent/../child"} {
		if err := preflightOutputDirectoryPath(root, path); !errors.Is(err, ErrPathEscape) {
			t.Fatalf("preflight unsafe path %q error = %v", path, err)
		}
		if opened, err := openOutputDirectoryPath(root, path, true); opened != nil || !errors.Is(err, ErrPathEscape) {
			t.Fatalf("open unsafe path %q = (directory=%v, err=%v)", path, opened, err)
		}
	}
}

func TestOutputV3AdmissionWalkPreservesAuthorityAndHandleClosure(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	if err := os.MkdirAll(filepath.Join(rootPath, "parent", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	faults, _, root := outputV3AdmissionWalkRoot(t, rootPath)
	createFailure := errors.New("create authority rejected")
	metadataFailure := errors.New("metadata authority rejected")
	faults.createErrors["parent"] = createFailure

	if err := preflightOutputDirectoryAuthorities(root, "parent/missing", false, false); !errors.Is(err, createFailure) {
		t.Fatalf("missing-suffix authority error = %v", err)
	}
	if opened, err := openOutputDirectoryPath(root, "parent/missing", true); opened != nil || !errors.Is(err, createFailure) {
		t.Fatalf("unauthorized create = (directory=%v, err=%v)", opened, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority failure created a directory: %v", err)
	}

	delete(faults.createErrors, "parent")
	faults.createErrors["parent/child"] = createFailure
	faults.metadataErrors["parent/child"] = metadataFailure
	err := preflightOutputDirectoryAuthorities(root, "parent/child", true, true)
	if !errors.Is(err, createFailure) || !errors.Is(err, metadataFailure) {
		t.Fatalf("final authority errors = %v", err)
	}
	delete(faults.createErrors, "parent/child")
	delete(faults.metadataErrors, "parent/child")

	closeFailure := errors.New("parent close failed")
	faults.closeErrors["parent"] = closeFailure
	closedBefore := faults.closeCalls["parent/child"]
	opened, err := openOutputDirectoryPath(root, "parent/child", false)
	if opened != nil || !errors.Is(err, closeFailure) {
		t.Fatalf("intermediate close failure = (directory=%v, err=%v)", opened, err)
	}
	if faults.closeCalls["parent/child"] != closedBefore+1 {
		t.Fatal("the next fixed handle leaked when its parent close failed")
	}
	delete(faults.closeErrors, "parent")

	finalCloseFailure := errors.New("final close failed")
	faults.closeErrors["parent/child"] = finalCloseFailure
	if err := preflightOutputDirectoryPath(root, "parent/child"); !errors.Is(err, finalCloseFailure) {
		t.Fatalf("preflight final close error = %v", err)
	}
}

func TestOutputV3MaterializationClassifiesAuthorityAndCloseFailures(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o700); err != nil {
		t.Fatal(err)
	}
	faults, _, root := outputV3AdmissionWalkRoot(t, rootPath)
	selection := outputV3AdmissionWalkSelection(t)

	metadataFailure := errors.New("directory metadata authority failed")
	faults.metadataErrors["parent/newdir"] = metadataFailure
	err := materializeOutputSelection(root, selection)
	if !errors.Is(err, metadataFailure) {
		t.Fatalf("directory metadata authority error = %v", err)
	}
	outputV3AdmissionRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false)
	delete(faults.metadataErrors, "parent/newdir")

	directoryCloseFailure := errors.New("selected directory close failed")
	faults.closeErrors["parent/newdir"] = directoryCloseFailure
	err = materializeOutputSelection(root, selection)
	if !errors.Is(err, directoryCloseFailure) {
		t.Fatalf("selected directory close error = %v", err)
	}
	outputV3AdmissionRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false)
	delete(faults.closeErrors, "parent/newdir")

	createFailure := errors.New("file parent create authority failed")
	faults.createErrors["parent"] = createFailure
	err = materializeOutputSelection(root, selection)
	if !errors.Is(err, createFailure) {
		t.Fatalf("selected file parent authority error = %v", err)
	}
	outputV3AdmissionRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false)
	delete(faults.createErrors, "parent")

	parentCloseFailure := errors.New("selected file parent close failed")
	faults.closeErrors["parent"] = parentCloseFailure
	err = materializeOutputSelection(root, selection)
	if !errors.Is(err, parentCloseFailure) {
		t.Fatalf("selected file parent close error = %v", err)
	}
	outputV3AdmissionRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false)
	delete(faults.closeErrors, "parent")

	if err := materializeOutputSelection(root, selection); err != nil {
		t.Fatalf("materialize admitted selection: %v", err)
	}
}

func TestOutputV3FrozenSelectionRejectionsAreFreshSessionFaults(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close static-admission platform: %v", err)
		}
	}()
	selection := outputV3AdmissionWalkSelection(t)
	modifiedTimeFailure := errors.New("selected modified time rejected")

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "reserved locator",
			run: func() error {
				return validateReservedOutputSelection(
					platform,
					v3RecoverySelectionPaths(t, []string{resumestate.ControlDirectoryName}, 1),
				)
			},
		},
		{
			name: "platform alias",
			run: func() error {
				_, err := preflightOutputSelectionAdmission(
					&outputV3AdmissionAliasPlatform{outputV3Platform: platform}, selection,
				)
				return err
			},
		},
		{
			name: "modified-time shape",
			run: func() error {
				_, err := preflightOutputSelectionAdmission(
					&outputV3AdmissionModifiedTimePlatform{
						outputV3Platform: platform,
						failure:          modifiedTimeFailure,
					},
					selection,
				)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			outputV3AdmissionRequireFault(
				t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false,
			)
		})
	}
}

func TestOutputV3FrozenSelectionRejectionsPauseMatchingIntent(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*outputV3AdmissionStaticFailure)
		wantCause error
	}{
		{
			name: "reserved locator",
			configure: func(failure *outputV3AdmissionStaticFailure) {
				failure.reservedAlias = true
			},
			wantCause: errReservedOutputPath,
		},
		{
			name: "platform alias",
			configure: func(failure *outputV3AdmissionStaticFailure) {
				failure.locatorAlias = true
			},
		},
		{
			name: "modified-time shape",
			configure: func(failure *outputV3AdmissionStaticFailure) {
				failure.modifiedTimeErr = errors.New("selected modified time rejected")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			selection := outputV3AdmissionWalkSelection(t)
			failure := &outputV3AdmissionStaticFailure{}
			authority := v3RecoveryAuthority(t, rootPath, nil)
			outputV3AdmissionInstallStaticFailurePlatform(authority, failure)
			initial := v3RecoveryOpen(t, authority, rootPath, selection)
			sessionID := initial.Session.SessionID()
			v3RecoveryCloseSession(t, initial.Session)

			test.configure(failure)
			opened, err := v3OpenSelection(context.Background(), authority, selection)
			if opened.Session != nil {
				v3RecoveryCloseSession(t, opened.Session)
				t.Fatal("matching-intent frozen-selection rejection returned a session")
			}
			outputV3AdmissionRequireFault(
				t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, true,
			)
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("frozen-selection error = %v, want cause %v", err, test.wantCause)
			}
			sessionPath := v3RecoverySessionPath(rootPath, selection, sessionID)
			if _, statErr := os.Stat(filepath.Join(sessionPath, resumestate.HeaderRecordName)); statErr != nil {
				t.Fatalf("matching session header was not preserved: %v", statErr)
			}
			if entries, readErr := os.ReadDir(filepath.Dir(sessionPath)); readErr != nil || len(entries) != 1 ||
				entries[0].Name() != resumestate.SessionDirectoryName(sessionID) {
				t.Fatalf("matching intent gained a competing session: entries=%v err=%v", entries, readErr)
			}

			*failure = outputV3AdmissionStaticFailure{}
			retried := v3RecoveryOpen(t, authority, rootPath, selection)
			if retried.Session.SessionID() != sessionID {
				t.Fatalf("frozen-selection retry session = %x, want %x", retried.Session.SessionID(), sessionID)
			}
			v3RecoveryCloseSession(t, retried.Session)
		})
	}
}

func TestOutputV3SelectedParentFailureUsesIntentLifecycleBoundary(t *testing.T) {
	t.Run("fresh selection", func(t *testing.T) {
		rootPath := v3RecoveryRoot(t)
		selection := outputV3AdmissionWalkSelection(t)
		parentPath := filepath.Join(rootPath, "parent")
		if err := os.WriteFile(parentPath, []byte("wrong type"), 0o600); err != nil {
			t.Fatal(err)
		}
		authority := v3RecoveryAuthority(t, rootPath, nil)

		opened, err := v3OpenSelection(context.Background(), authority, selection)
		if opened.Session != nil {
			v3RecoveryCloseSession(t, opened.Session)
			t.Fatal("fresh selected-parent rejection returned a session")
		}
		outputV3AdmissionRequireFault(
			t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false,
		)
		if _, statErr := os.Stat(filepath.Join(rootPath, resumestate.ControlDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("fresh selected-parent rejection created control state: %v", statErr)
		}

		if err := os.Remove(parentPath); err != nil {
			t.Fatal(err)
		}
		retried := v3RecoveryOpen(t, authority, rootPath, selection)
		v3RecoveryCloseSession(t, retried.Session)
	})

	t.Run("matching intent", func(t *testing.T) {
		rootPath := v3RecoveryRoot(t)
		selection := outputV3AdmissionWalkSelection(t)
		authority := v3RecoveryAuthority(t, rootPath, nil)
		initial := v3RecoveryOpen(t, authority, rootPath, selection)
		sessionID := initial.Session.SessionID()
		v3RecoveryCloseSession(t, initial.Session)

		parentPath := filepath.Join(rootPath, "parent")
		displacedPath := filepath.Join(rootPath, "parent.admitted")
		if err := os.Rename(parentPath, displacedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parentPath, []byte("wrong type"), 0o600); err != nil {
			t.Fatal(err)
		}

		opened, err := v3OpenSelection(context.Background(), authority, selection)
		if opened.Session != nil {
			v3RecoveryCloseSession(t, opened.Session)
			t.Fatal("matching-intent selected-parent rejection returned a session")
		}
		outputV3AdmissionRequireFault(
			t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, true,
		)

		sessionPath := v3RecoverySessionPath(rootPath, selection, sessionID)
		if _, statErr := os.Stat(filepath.Join(sessionPath, resumestate.HeaderRecordName)); statErr != nil {
			t.Fatalf("matching session header was not preserved: %v", statErr)
		}
		intentPath := filepath.Dir(sessionPath)
		if entries, readErr := os.ReadDir(intentPath); readErr != nil || len(entries) != 1 ||
			entries[0].Name() != resumestate.SessionDirectoryName(sessionID) {
			t.Fatalf("matching intent gained a competing session: entries=%v err=%v", entries, readErr)
		}

		if err := os.Remove(parentPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(displacedPath, parentPath); err != nil {
			t.Fatal(err)
		}
		retried := v3RecoveryOpen(t, authority, rootPath, selection)
		if retried.Session.SessionID() != sessionID {
			t.Fatalf("restored selected parent session = %x, want %x", retried.Session.SessionID(), sessionID)
		}
		v3RecoveryCloseSession(t, retried.Session)
	})
}

func TestOutputV3AuthorityPreflightSeparatesRootAndSelectedDescendantFaults(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*outputV3AdmissionWalkFaults, error)
		wantScope transfer.OutputFaultScope
		wantPause bool
	}{
		{
			name: "exact output root",
			configure: func(faults *outputV3AdmissionWalkFaults, failure error) {
				faults.createErrors[""] = failure
			},
			wantScope: transfer.OutputFaultRoot,
			wantPause: true,
		},
		{
			name: "selected descendant",
			configure: func(faults *outputV3AdmissionWalkFaults, failure error) {
				faults.metadataErrors["parent"] = failure
			},
			wantScope: transfer.OutputFaultSession,
			wantPause: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o700); err != nil {
				t.Fatal(err)
			}
			selection := outputV3AdmissionWalkSelection(t)
			authority := v3RecoveryAuthority(t, rootPath, nil)
			faults := &outputV3AdmissionWalkFaults{
				createErrors: make(map[string]error), metadataErrors: make(map[string]error),
				closeErrors: make(map[string]error), closeCalls: make(map[string]int),
			}
			failure := errors.New("injected authority failure")
			test.configure(faults, failure)
			outputV3AdmissionInstallWalkPlatform(authority, faults)

			opened, err := v3OpenSelection(context.Background(), authority, selection)
			if opened.Session != nil {
				v3RecoveryCloseSession(t, opened.Session)
				t.Fatal("authority rejection returned a session")
			}
			if !errors.Is(err, failure) {
				t.Fatalf("authority rejection error = %v, want injected failure", err)
			}
			outputV3SemanticRequireFault(
				t, err, test.wantScope, transfer.OutputFaultNamespaceUnsafe,
			)
			var sessionErr *transfer.OutputSessionError
			if errors.As(err, &sessionErr) {
				t.Fatalf("fresh authority rejection requested an explicit session pause: %v", err)
			}
			var requirement interface{ RequiresJobPause() bool }
			if !errors.As(err, &requirement) || requirement.RequiresJobPause() != test.wantPause {
				t.Fatalf("authority rejection pause requirement = %v, want %t", requirement, test.wantPause)
			}
		})
	}
}

func TestOutputV3SelectionMetadataFailureUsesIntentLifecycleBoundary(t *testing.T) {
	t.Run("fresh selection", func(t *testing.T) {
		rootPath := v3RecoveryRoot(t)
		selection := outputV3AdmissionWalkSelection(t)
		failure := &outputV3AdmissionMetadataFailure{err: errors.New("metadata witness rejected")}
		authority := v3RecoveryAuthority(t, rootPath, nil)
		outputV3AdmissionInstallMetadataPlatform(authority, failure)

		opened, err := v3OpenSelection(context.Background(), authority, selection)
		if opened.Session != nil {
			v3RecoveryCloseSession(t, opened.Session)
			t.Fatal("fresh metadata-witness rejection returned a session")
		}
		outputV3AdmissionRequireFault(
			t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, false,
		)
		if _, statErr := os.Stat(filepath.Join(rootPath, resumestate.ControlDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("fresh metadata-witness rejection created control state: %v", statErr)
		}
	})

	t.Run("matching intent", func(t *testing.T) {
		rootPath := v3RecoveryRoot(t)
		selection := outputV3AdmissionWalkSelection(t)
		failure := &outputV3AdmissionMetadataFailure{}
		authority := v3RecoveryAuthority(t, rootPath, nil)
		outputV3AdmissionInstallMetadataPlatform(authority, failure)
		initial := v3RecoveryOpen(t, authority, rootPath, selection)
		sessionID := initial.Session.SessionID()
		v3RecoveryCloseSession(t, initial.Session)

		failure.err = errors.New("metadata witness rejected")
		opened, err := v3OpenSelection(context.Background(), authority, selection)
		if opened.Session != nil {
			v3RecoveryCloseSession(t, opened.Session)
			t.Fatal("matching-intent metadata-witness rejection returned a session")
		}
		outputV3AdmissionRequireFault(
			t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, true,
		)
		if _, statErr := os.Stat(filepath.Join(
			v3RecoverySessionPath(rootPath, selection, sessionID), resumestate.HeaderRecordName,
		)); statErr != nil {
			t.Fatalf("matching session header was not preserved: %v", statErr)
		}

		failure.err = nil
		retried := v3RecoveryOpen(t, authority, rootPath, selection)
		if retried.Session.SessionID() != sessionID {
			t.Fatalf("metadata-witness retry session = %x, want %x", retried.Session.SessionID(), sessionID)
		}
		v3RecoveryCloseSession(t, retried.Session)
	})
}

type outputV3AdmissionWalkFaults struct {
	createErrors   map[string]error
	metadataErrors map[string]error
	closeErrors    map[string]error
	closeCalls     map[string]int
}

type outputV3AdmissionWalkDirectory struct {
	outputV3Directory
	path   string
	faults *outputV3AdmissionWalkFaults
}

func (directory *outputV3AdmissionWalkDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &outputV3AdmissionWalkDirectory{
		outputV3Directory: duplicate, path: directory.path, faults: directory.faults,
	}, nil
}

func (directory *outputV3AdmissionWalkDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3AdmissionWalkDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

func (directory *outputV3AdmissionWalkDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return directory.wrap(opened, name), nil
}

func (directory *outputV3AdmissionWalkDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return directory.wrap(created, name), nil
}

func (directory *outputV3AdmissionWalkDirectory) Close() error {
	directory.faults.closeCalls[directory.path]++
	return errors.Join(directory.outputV3Directory.Close(), directory.faults.closeErrors[directory.path])
}

func (directory *outputV3AdmissionWalkDirectory) ValidateCreateAuthority() error {
	return directory.faults.createErrors[directory.path]
}

func (directory *outputV3AdmissionWalkDirectory) ValidateMetadataAuthority() error {
	return directory.faults.metadataErrors[directory.path]
}

func (directory *outputV3AdmissionWalkDirectory) wrap(
	child outputV3Directory,
	name string,
) outputV3Directory {
	path := name
	if directory.path != "" {
		path = directory.path + "/" + name
	}
	return &outputV3AdmissionWalkDirectory{outputV3Directory: child, path: path, faults: directory.faults}
}

type outputV3AdmissionWalkPlatform struct {
	outputV3Platform
	root outputV3Directory
}

func (platform *outputV3AdmissionWalkPlatform) Root() outputV3Directory { return platform.root }

func (platform *outputV3AdmissionWalkPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*outputV3AdmissionWalkDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return &outputV3AdmissionWalkDirectory{
				outputV3Directory: root,
				path:              decorated.path,
				faults:            decorated.faults,
			}
		},
	)
}

func outputV3AdmissionWalkRoot(
	t *testing.T,
	rootPath string,
) (*outputV3AdmissionWalkFaults, outputV3Platform, outputV3Directory) {
	t.Helper()
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close admission platform: %v", err)
		}
	})
	faults := &outputV3AdmissionWalkFaults{
		createErrors: make(map[string]error), metadataErrors: make(map[string]error),
		closeErrors: make(map[string]error), closeCalls: make(map[string]int),
	}
	return faults, platform, &outputV3AdmissionWalkDirectory{
		outputV3Directory: platform.Root(), faults: faults,
	}
}

func outputV3AdmissionWalkSelection(t *testing.T) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x21)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x22)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x23)
	modified := v3RecoveryModifiedTime(t)
	parent := transfer.OutputSelectionDirectory{
		Path: "parent", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x24),
		Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x25), ModifiedTime: modified,
	}
	directories := []transfer.OutputSelectionDirectory{parent, {
		Path: "parent/newdir", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x26),
		Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x27), ModifiedTime: modified,
	}}
	files := []transfer.OutputSelectionFile{{
		Path: "parent/output.bin", FileID: v3RecoveryIdentity16[catalog.FileID](0x28),
		ParentDirectoryID: parent.DirectoryID, ParentGeneration: parent.Generation,
		ExpectedSize: 1, ModifiedTime: modified,
	}}
	plan, err := transfer.NewOutputSelection(share, root, rootGeneration, directories, files)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

type outputV3AdmissionAliasPlatform struct{ outputV3Platform }

func (*outputV3AdmissionAliasPlatform) CanonicalLocatorKey(string) (string, error) {
	return "platform-equivalent-locator", nil
}

type outputV3AdmissionModifiedTimePlatform struct {
	outputV3Platform
	failure error
}

func (platform *outputV3AdmissionModifiedTimePlatform) ValidateModifiedTime(catalog.ModifiedTime) error {
	return platform.failure
}

type outputV3AdmissionStaticFailure struct {
	reservedAlias   bool
	locatorAlias    bool
	modifiedTimeErr error
}

type outputV3AdmissionStaticFailurePlatform struct {
	outputV3Platform
	failure *outputV3AdmissionStaticFailure
}

func (platform *outputV3AdmissionStaticFailurePlatform) CanonicalComponentKey(component string) (string, error) {
	if platform.failure.reservedAlias {
		return "platform-equivalent-component", nil
	}
	return platform.outputV3Platform.CanonicalComponentKey(component)
}

func (platform *outputV3AdmissionStaticFailurePlatform) CanonicalLocatorKey(locator string) (string, error) {
	if platform.failure.locatorAlias {
		return "platform-equivalent-locator", nil
	}
	return platform.outputV3Platform.CanonicalLocatorKey(locator)
}

func (platform *outputV3AdmissionStaticFailurePlatform) ValidateModifiedTime(
	modified catalog.ModifiedTime,
) error {
	if platform.failure.modifiedTimeErr != nil {
		return platform.failure.modifiedTimeErr
	}
	return platform.outputV3Platform.ValidateModifiedTime(modified)
}

type outputV3AdmissionMetadataFailure struct{ err error }

type outputV3AdmissionMetadataPlatform struct {
	outputV3Platform
	failure *outputV3AdmissionMetadataFailure
}

func (platform *outputV3AdmissionMetadataPlatform) ValidateSelectionMetadata(
	selection transfer.OutputSelection,
) error {
	if platform.failure.err != nil {
		return platform.failure.err
	}
	return platform.outputV3Platform.ValidateSelectionMetadata(selection)
}

func outputV3AdmissionInstallWalkPlatform(
	authority *FilesystemOutputAuthority,
	faults *outputV3AdmissionWalkFaults,
) {
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		root := &outputV3AdmissionWalkDirectory{
			outputV3Directory: platform.Root(),
			faults:            faults,
		}
		return &outputV3AdmissionWalkPlatform{outputV3Platform: platform, root: root}, nil
	}
}

func outputV3AdmissionInstallMetadataPlatform(
	authority *FilesystemOutputAuthority,
	failure *outputV3AdmissionMetadataFailure,
) {
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &outputV3AdmissionMetadataPlatform{
			outputV3Platform: platform,
			failure:          failure,
		}, nil
	}
}

func outputV3AdmissionInstallStaticFailurePlatform(
	authority *FilesystemOutputAuthority,
	failure *outputV3AdmissionStaticFailure,
) {
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &outputV3AdmissionStaticFailurePlatform{
			outputV3Platform: platform,
			failure:          failure,
		}, nil
	}
}

func outputV3AdmissionRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
	wantExplicitPause bool,
) {
	t.Helper()
	outputV3SemanticRequireFault(t, err, scope, code)
	var sessionErr *transfer.OutputSessionError
	if got := errors.As(err, &sessionErr); got != wantExplicitPause {
		t.Fatalf("explicit output-session pause wrapper = %t, want %t: %v", got, wantExplicitPause, err)
	}
	if sessionErr != nil && !sessionErr.RequiresJobPause() {
		t.Fatalf("explicit output-session wrapper did not require preservation pause: %v", err)
	}
	var requirement interface{ RequiresJobPause() bool }
	if !errors.As(err, &requirement) {
		t.Fatalf("output fault lacks a pause disposition: %v", err)
	}
	if got := requirement.RequiresJobPause(); got != wantExplicitPause {
		t.Fatalf("effective pause requirement = %t, want %t: %v", got, wantExplicitPause, err)
	}
}

var _ outputV3CreateAuthorityValidator = (*outputV3AdmissionWalkDirectory)(nil)
var _ outputV3MetadataAuthorityValidator = (*outputV3AdmissionWalkDirectory)(nil)
