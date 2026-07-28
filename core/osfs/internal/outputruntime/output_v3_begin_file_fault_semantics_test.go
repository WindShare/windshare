package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3BeginFileFailsBeforeContentWhenAuthorityCannotBeProven(t *testing.T) {
	failure := errors.New("begin-file authority fault")
	for _, test := range []struct {
		name      string
		configure func(*testing.T, *Session, transfer.OutputFile)
		cause     error
		scope     transfer.OutputFaultScope
		code      transfer.OutputFaultCode
	}{
		{
			name: "closed session",
			configure: func(t *testing.T, session *Session, _ transfer.OutputFile) {
				session.mu.Lock()
				session.closed = true
				session.mu.Unlock()
				t.Cleanup(func() {
					session.mu.Lock()
					session.closed = false
					session.mu.Unlock()
				})
			},
			cause: outputfault.ErrSessionClosed, scope: transfer.OutputFaultSession, code: transfer.OutputFaultOwnership,
		},
		{
			name: "file-state shard inspection",
			configure: func(t *testing.T, session *Session, _ transfer.OutputFile) {
				original := session.filesDir
				session.filesDir = &outputV3BeginFileStateDirectory{
					Directory: original, classifyErr: failure,
				}
				t.Cleanup(func() { session.filesDir = original })
			},
			cause: failure, code: transfer.OutputFaultStateIO,
		},
		{
			name: "final parent reopen",
			configure: func(t *testing.T, session *Session, _ transfer.OutputFile) {
				outputV3InstallBeginFileRootFault(t, session, &outputV3PublicationDirectoryFaults{prepareIdentityErr: failure})
			},
			cause: failure, scope: transfer.OutputFaultSession, code: transfer.OutputFaultStateIO,
		},
		{
			name: "final entry observation",
			configure: func(t *testing.T, session *Session, _ transfer.OutputFile) {
				outputV3InstallBeginFileRootFault(t, session, &outputV3PublicationDirectoryFaults{observeErr: failure})
			},
			cause: failure, code: transfer.OutputFaultStateIO,
		},
		{
			name: "final parent close",
			configure: func(t *testing.T, session *Session, _ transfer.OutputFile) {
				outputV3InstallBeginFileGuardCloseFault(t, session, failure)
			},
			cause: failure, scope: transfer.OutputFaultSession, code: transfer.OutputFaultStateIO,
		},
		{
			name: "file-state shard creation",
			configure: func(t *testing.T, session *Session, _ transfer.OutputFile) {
				original := session.filesDir
				session.filesDir = &outputV3BeginFileStateDirectory{
					Directory: original, createDirectoryErr: failure,
				}
				t.Cleanup(func() { session.filesDir = original })
			},
			cause: failure, code: transfer.OutputFaultStateIO,
		},
		{
			name: "output object allocation",
			configure: func(_ *testing.T, session *Session, _ transfer.OutputFile) {
				session.owner.objectIDs = outputV3BeginFileObjectIDs{err: failure}
			},
			cause: failure, code: transfer.OutputFaultStateIO,
		},
		{
			name: "initial file record installation",
			configure: func(t *testing.T, session *Session, file transfer.OutputFile) {
				recordName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(file.Path))
				shard, _, err := openOutputShard(session.filesDir, recordName.Shard(), true)
				if err != nil {
					t.Fatal(err)
				}
				if err := shard.Close(); err != nil {
					t.Fatal(err)
				}
				original := session.filesDir
				session.filesDir = &outputV3BeginFileStateDirectory{
					Directory: original, childCreateFileErr: failure,
				}
				t.Cleanup(func() { session.filesDir = original })
			},
			cause: failure, code: transfer.OutputFaultStateIO,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			test.configure(t, opened.Session, file)

			start, err := opened.Session.BeginFile(context.Background(), file)
			if _, _, started := start.Transaction(); started {
				t.Fatal("authority failure created a content transaction")
			}
			if !errors.Is(err, test.cause) {
				t.Fatalf("begin-file authority error = %v", err)
			}
			scope := test.scope
			if scope == 0 {
				scope = transfer.OutputFaultFile
			}
			outputV3SemanticRequireFault(t, err, scope, test.code)
		})
	}
}

func TestOutputV3DirectoryLifecyclePropagatesSessionAndNamespaceFaults(t *testing.T) {
	failure := errors.New("directory lifecycle fault")
	root := v3RecoveryRoot(t)
	selection := v3SemanticNestedSelection(t, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	selected := selection.Directories()[0]
	directory := transfer.OutputDirectory{Path: selected.Path, ModifiedTime: selected.ModifiedTime}

	for _, operation := range []struct {
		name string
		call func(context.Context, transfer.OutputDirectory) error
	}{
		{name: "finalize", call: opened.Session.FinalizeDirectory},
	} {
		t.Run(operation.name+" closed session", func(t *testing.T) {
			opened.Session.mu.Lock()
			opened.Session.closed = true
			opened.Session.mu.Unlock()
			err := operation.call(context.Background(), directory)
			opened.Session.mu.Lock()
			opened.Session.closed = false
			opened.Session.mu.Unlock()
			if !errors.Is(err, outputfault.ErrSessionClosed) {
				t.Fatalf("closed directory lifecycle error = %v", err)
			}
		})

		t.Run(operation.name+" namespace reopen", func(t *testing.T) {
			outputV3InstallBeginFileRootFault(
				t, opened.Session, &outputV3PublicationDirectoryFaults{openDirectoryErr: failure},
			)
			err := operation.call(context.Background(), directory)
			if !errors.Is(err, failure) {
				t.Fatalf("directory namespace error = %v", err)
			}
		})
	}
}

func TestOutputV3ObjectAllocationBudgetRejectsNonAuthorityIDs(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	opened.Session.owner.objectIDs = &outputV3SemanticObjectIDs{
		values: make([]resumestate.OutputObjectID, outputnamespace.AllocationAttempts),
	}
	if _, err := opened.Session.allocateOutputObjectID(
		resumestate.DigestCanonicalLocator("budget.bin"),
	); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("object allocation budget error = %v", err)
	}
}

type outputV3BeginFileStateDirectory struct {
	outputcap.Directory
	classifyErr        error
	createDirectoryErr error
	childCreateFileErr error
}

func (directory *outputV3BeginFileStateDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if directory.classifyErr != nil {
		return outputcap.EntryAbsent, false, directory.classifyErr
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *outputV3BeginFileStateDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.createDirectoryErr != nil {
		return nil, directory.createDirectoryErr
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3BeginFileRecordDirectory{
		Directory:     created,
		createFileErr: directory.childCreateFileErr,
	}, nil
}

func (directory *outputV3BeginFileStateDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3BeginFileRecordDirectory{
		Directory:     opened,
		createFileErr: directory.childCreateFileErr,
	}, nil
}

type outputV3BeginFileRecordDirectory struct {
	outputcap.Directory
	createFileErr error
}

func (directory *outputV3BeginFileRecordDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if directory.createFileErr != nil {
		return nil, directory.createFileErr
	}
	return directory.Directory.CreateFile(name, private, size)
}

type outputV3BeginFileObjectIDs struct{ err error }

func (ids outputV3BeginFileObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	return resumestate.OutputObjectID{}, ids.err
}

func outputV3InstallBeginFileRootFault(
	t *testing.T,
	session *Session,
	faults *outputV3PublicationDirectoryFaults,
) {
	t.Helper()
	original := session.platform
	session.platform = &outputV3PublicationPlatform{
		Platform: original,
		root: &outputV3PublicationDirectory{
			Directory: original.Root(),
			faults:    faults,
		},
	}
	t.Cleanup(func() { session.platform = original })
}

func outputV3InstallBeginFileGuardCloseFault(
	t *testing.T,
	session *Session,
	failure error,
) {
	t.Helper()
	original := session.platform
	faults := &outputV3PublicationDirectoryFaults{}
	session.platform = &outputV3PublicationPlatform{
		Platform: original,
		root: &outputV3PublicationDirectory{
			Directory: original.Root(),
			faults:    faults,
		},
		guardCloseErr: failure,
	}
	t.Cleanup(func() { session.platform = original })
}
