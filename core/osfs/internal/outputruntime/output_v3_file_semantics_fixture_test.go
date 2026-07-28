package outputruntime

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type outputV3SemanticFaultFile struct {
	outputcap.File
	shortWrite       bool
	syncErr          error
	syncCalls        int
	setModifiedErr   error
	setModifiedCalls int
	forceDifferent   bool
	sameErr          error
	closeErr         error
	closeCalls       int
}

func (file *outputV3SemanticFaultFile) WriteAt(data []byte, offset int64) (int, error) {
	if file.shortWrite {
		return max(0, len(data)-1), nil
	}
	return file.File.WriteAt(data, offset)
}

func (file *outputV3SemanticFaultFile) Sync() error {
	file.syncCalls++
	if file.syncErr != nil {
		return file.syncErr
	}
	return file.File.Sync()
}

func (file *outputV3SemanticFaultFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	file.setModifiedCalls++
	if file.setModifiedErr != nil {
		return file.setModifiedErr
	}
	return file.File.SetModifiedTime(modified)
}

func (file *outputV3SemanticFaultFile) SameFile(other outputcap.File) (bool, error) {
	if file.sameErr != nil {
		return false, file.sameErr
	}
	if file.forceDifferent {
		return false, nil
	}
	return file.File.SameFile(other)
}

func (file *outputV3SemanticFaultFile) Close() error {
	file.closeCalls++
	if file.File == nil {
		return file.closeErr
	}
	return errors.Join(file.File.Close(), file.closeErr)
}

type outputV3SemanticFaultDirectory struct {
	outputcap.Directory
	createFileErr error
}

func (directory *outputV3SemanticFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if directory.createFileErr != nil {
		return nil, directory.createFileErr
	}
	return directory.Directory.CreateFile(name, private, size)
}

type outputV3WitnessCreationDirectory struct {
	outputcap.Directory
	syncErr        error
	syncErrAt      int
	syncCalls      *int
	linkedCloseErr error
}

func (directory *outputV3WitnessCreationDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return opened, err
	}
	return directory.wrap(opened), nil
}

func (directory *outputV3WitnessCreationDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return created, err
	}
	return directory.wrap(created), nil
}

func (directory *outputV3WitnessCreationDirectory) LinkFileNoReplace(
	source outputcap.File,
	name string,
) (outputcap.File, error) {
	linked, err := directory.Directory.LinkFileNoReplace(source, name)
	if linked == nil {
		return nil, err
	}
	return &outputV3WitnessCreationFile{
		File:     linked,
		closeErr: directory.linkedCloseErr,
	}, err
}

func (directory *outputV3WitnessCreationDirectory) Sync() error {
	(*directory.syncCalls)++
	if directory.syncErr != nil && *directory.syncCalls == directory.syncErrAt {
		return directory.syncErr
	}
	return directory.Directory.Sync()
}

func (directory *outputV3WitnessCreationDirectory) wrap(
	child outputcap.Directory,
) *outputV3WitnessCreationDirectory {
	return &outputV3WitnessCreationDirectory{
		Directory:      child,
		syncErr:        directory.syncErr,
		syncErrAt:      directory.syncErrAt,
		syncCalls:      directory.syncCalls,
		linkedCloseErr: directory.linkedCloseErr,
	}
}

type outputV3WitnessCreationFile struct {
	outputcap.File
	closeErr error
}

func (file *outputV3WitnessCreationFile) Close() error {
	return errors.Join(file.File.Close(), file.closeErr)
}

// outputV3SemanticFaultShardParent models a fixed parent whose child changes
// after recovery observation but before transactionStart reopens the witness.
type outputV3SemanticFaultShardParent struct {
	outputcap.Directory
	absentShard         string
	targetShard         string
	childOpenFileErr    error
	childForceDifferent bool
}

func (directory *outputV3SemanticFaultShardParent) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if name == directory.absentShard {
		return outputcap.EntryAbsent, true, nil
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *outputV3SemanticFaultShardParent) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil || name != directory.targetShard {
		return opened, err
	}
	return &outputV3SemanticFaultShard{
		Directory:      opened,
		openFileErr:    directory.childOpenFileErr,
		forceDifferent: directory.childForceDifferent,
	}, nil
}

type outputV3SemanticFaultShard struct {
	outputcap.Directory
	openFileErr    error
	forceDifferent bool
}

func (directory *outputV3SemanticFaultShard) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if directory.openFileErr != nil {
		return nil, directory.openFileErr
	}
	opened, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil || !directory.forceDifferent {
		return opened, err
	}
	return &outputV3SemanticFaultFile{File: opened, forceDifferent: true}, nil
}

type outputV3SemanticObjectIDs struct {
	values []resumestate.OutputObjectID
	next   int
}

func (ids *outputV3SemanticObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	if ids.next >= len(ids.values) {
		return resumestate.OutputObjectID{}, io.EOF
	}
	value := ids.values[ids.next]
	ids.next++
	return value, nil
}

func outputV3SemanticInstallReservedRecord(
	t *testing.T,
	session *Session,
	file transfer.OutputFile,
) resumestate.ResumableFileAuthority {
	t.Helper()
	digest := resumestate.DigestCanonicalLocator(file.Path)
	objectID, err := session.allocateOutputObjectID(digest)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: session.stateSnapshot(), Descriptor: file.Descriptor,
		CanonicalLocator: file.Path, OutputObject: objectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	installFileNamespaceTestRecord(t, session, authority.Bound())
	return authority
}

func outputV3SemanticOpenShard(
	t *testing.T,
	parent outputcap.Directory,
	name string,
	create bool,
) outputcap.Directory {
	t.Helper()
	shard, present, err := openOutputShard(parent, name, create)
	if err != nil || !present {
		t.Fatalf("open shard %q = (present=%t, err=%v)", name, present, err)
	}
	return shard
}

func outputV3SemanticCreatePrivateFile(
	t *testing.T,
	parent outputcap.Directory,
	name resumestate.ShardedName,
	payload []byte,
	size int64,
) {
	t.Helper()
	outputV3SemanticCreatePrivateNamedFile(t, parent, name.Shard(), name.Name(), payload, size)
}

func outputV3SemanticCreatePrivateNamedFile(
	t *testing.T,
	parent outputcap.Directory,
	shardName string,
	fileName string,
	payload []byte,
	size int64,
) {
	t.Helper()
	shard := outputV3SemanticOpenShard(t, parent, shardName, true)
	file, err := shard.CreateFile(fileName, true, size)
	if err != nil {
		_ = shard.Close()
		t.Fatal(err)
	}
	if len(payload) != 0 {
		written, writeErr := file.WriteAt(payload, 0)
		if writeErr != nil || written != len(payload) {
			_ = file.Close()
			_ = shard.Close()
			t.Fatalf("write private file = (%d, %v)", written, writeErr)
		}
	}
	if err := errors.Join(file.Sync(), shard.Sync(), file.Close(), shard.Close()); err != nil {
		t.Fatal(err)
	}
}

func outputV3SemanticRequireRecordAbsent(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	locator string,
) {
	t.Helper()
	name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(locator))
	path := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.FilesDirectoryName, name.Shard(), name.Name(),
	)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file record %q stat error = %v, want absent", locator, err)
	}
}

func outputV3SemanticDetachTransaction(
	t *testing.T,
	session *Session,
	transaction *FileTransaction,
) {
	t.Helper()
	record := transaction.resumable.Bound().Record()
	transaction.lifecycle = FileTransactionClosed
	if err := transaction.closeHandles(); err != nil {
		t.Fatal(err)
	}
	session.finishFile(record.LocatorDigest(), transaction)
}

func outputV3SemanticRequireCheckpointState(
	t *testing.T,
	transaction *FileTransaction,
	wantGeneration uint64,
	wantPending int,
) {
	t.Helper()
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	record := transaction.resumable.Bound().Record()
	if record.CheckpointGeneration() != wantGeneration || transaction.pending.Len() != wantPending {
		t.Fatalf(
			"checkpoint authority = generation:%d pending:%v, want generation:%d pending:%d",
			record.CheckpointGeneration(), transaction.pending.Ranges(), wantGeneration, wantPending,
		)
	}
}

func outputV3SemanticRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %v, want scope=%v code=%v", err, scope, code)
	}
}

func outputV3SemanticObjectID(t *testing.T, value byte) resumestate.OutputObjectID {
	t.Helper()
	id, err := resumestate.OutputObjectIDFromBytes(bytes.Repeat([]byte{value}, resumestate.OutputObjectIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
