package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type outputV3PublicationFileFaults struct {
	sizeErr          error
	sizeAdjustment   int64
	sameErrAt        int
	sameErr          error
	differentAt      int
	metadataErr      error
	metadataMismatch bool
	closeErr         error
}

func (faults outputV3PublicationFileFaults) hasInjectedError() bool {
	return faults.sizeErr != nil || faults.sameErr != nil || faults.metadataErr != nil || faults.closeErr != nil
}

type outputV3PublicationFile struct {
	outputcap.File
	faults    outputV3PublicationFileFaults
	sameCalls int
}

func (file *outputV3PublicationFile) Size() (uint64, error) {
	if file.faults.sizeErr != nil {
		return 0, file.faults.sizeErr
	}
	size, err := file.File.Size()
	if err != nil || file.faults.sizeAdjustment == 0 {
		return size, err
	}
	if file.faults.sizeAdjustment > 0 {
		return size + uint64(file.faults.sizeAdjustment), nil
	}
	return size - uint64(-file.faults.sizeAdjustment), nil
}

func (file *outputV3PublicationFile) SameFile(other outputcap.File) (bool, error) {
	file.sameCalls++
	if file.sameCalls == file.faults.sameErrAt {
		return false, file.faults.sameErr
	}
	if file.sameCalls == file.faults.differentAt {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3PublicationFile); ok {
		other = wrapped.File
	}
	return file.File.SameFile(other)
}

func (file *outputV3PublicationFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file.faults.metadataErr != nil {
		return false, file.faults.metadataErr
	}
	if file.faults.metadataMismatch {
		return false, nil
	}
	return file.File.MetadataMatches(size, modified)
}

func (file *outputV3PublicationFile) Close() error {
	return errors.Join(file.File.Close(), file.faults.closeErr)
}

func unwrapOutputV3PublicationFile(file outputcap.File) outputcap.File {
	if wrapped, ok := file.(*outputV3PublicationFile); ok {
		return wrapped.File
	}
	return file
}

type outputV3PublicationOpenDirectory struct {
	outputcap.Directory
	openErr    error
	fileFaults outputV3PublicationFileFaults
}

func (directory *outputV3PublicationOpenDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if directory.openErr != nil {
		return nil, directory.openErr
	}
	opened, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationFile{File: opened, faults: directory.fileFaults}, nil
}

type outputV3PublicationDirectoryFaults struct {
	duplicateErr       error
	duplicateErrAt     int
	duplicateCalls     int
	prepareIdentityErr error
	identityErr        error
	sameDirectoryAt    int
	sameDirectoryCalls int
	openDirectoryErr   error
	createDirectoryErr error
	createFileErr      error
	createdFaults      *outputV3PublicationFileFaults
	linkErr            error
	linkReturnsNil     bool
	linkedFaults       outputV3PublicationFileFaults
	observeErr         error
	observeKind        outputcap.EntryKind
	openErr            error
	openedFaults       outputV3PublicationFileFaults
	syncErr            error
	modifiedErr        error
	closeErr           error
}

type outputV3PublicationDirectory struct {
	outputcap.Directory
	faults *outputV3PublicationDirectoryFaults
}

func (directory *outputV3PublicationDirectory) Duplicate() (outputcap.Directory, error) {
	directory.faults.duplicateCalls++
	if directory.faults.duplicateErr != nil &&
		(directory.faults.duplicateErrAt == 0 || directory.faults.duplicateCalls == directory.faults.duplicateErrAt) {
		return nil, directory.faults.duplicateErr
	}
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationDirectory{Directory: duplicate, faults: directory.faults}, nil
}

func (directory *outputV3PublicationDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	directory.faults.sameDirectoryCalls++
	if directory.faults.sameDirectoryAt != 0 &&
		directory.faults.sameDirectoryCalls == directory.faults.sameDirectoryAt {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3PublicationDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *outputV3PublicationDirectory) PrepareIdentityClaim() (outputcap.PersistentDirectoryIdentity, error) {
	if directory.faults.prepareIdentityErr != nil {
		return outputcap.PersistentDirectoryIdentity{}, directory.faults.prepareIdentityErr
	}
	return directory.Directory.PrepareIdentityClaim()
}

func (directory *outputV3PublicationDirectory) IdentityClaim() (outputcap.PersistentDirectoryIdentity, error) {
	if directory.faults.identityErr != nil {
		return outputcap.PersistentDirectoryIdentity{}, directory.faults.identityErr
	}
	return directory.Directory.IdentityClaim()
}

func (directory *outputV3PublicationDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.faults.openDirectoryErr != nil {
		return nil, directory.faults.openDirectoryErr
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationDirectory{Directory: opened, faults: directory.faults}, nil
}

func (directory *outputV3PublicationDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.faults.createDirectoryErr != nil {
		return nil, directory.faults.createDirectoryErr
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationDirectory{Directory: created, faults: directory.faults}, nil
}

func (directory *outputV3PublicationDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if directory.faults.createFileErr != nil {
		return nil, directory.faults.createFileErr
	}
	created, err := directory.Directory.CreateFile(name, private, size)
	if err != nil {
		return nil, err
	}
	faults := directory.faults.openedFaults
	if directory.faults.createdFaults != nil {
		faults = *directory.faults.createdFaults
	}
	return &outputV3PublicationFile{File: created, faults: faults}, nil
}

func (directory *outputV3PublicationDirectory) LinkFileNoReplace(
	source outputcap.File,
	name string,
) (outputcap.File, error) {
	if directory.faults.linkErr != nil {
		return nil, directory.faults.linkErr
	}
	if directory.faults.linkReturnsNil {
		return nil, nil
	}
	linked, err := directory.Directory.LinkFileNoReplace(unwrapOutputV3PublicationFile(source), name)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationFile{File: linked, faults: directory.faults.linkedFaults}, nil
}

func (directory *outputV3PublicationDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	return directory.Directory.ReplacePrivateFile(unwrapOutputV3PublicationFile(source), name)
}

func (directory *outputV3PublicationDirectory) RemoveFile(name string, expected outputcap.File) error {
	return directory.Directory.RemoveFile(name, unwrapOutputV3PublicationFile(expected))
}

func (directory *outputV3PublicationDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory.faults.observeErr != nil {
		return outputcap.EntryAbsent, directory.faults.observeErr
	}
	if directory.faults.observeKind != outputcap.EntryAbsent {
		return directory.faults.observeKind, nil
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *outputV3PublicationDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if directory.faults.openErr != nil {
		return nil, directory.faults.openErr
	}
	opened, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationFile{File: opened, faults: directory.faults.openedFaults}, nil
}

func (directory *outputV3PublicationDirectory) Sync() error {
	if directory.faults.syncErr != nil {
		return directory.faults.syncErr
	}
	return directory.Directory.Sync()
}

func (directory *outputV3PublicationDirectory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory.faults.modifiedErr != nil {
		return directory.faults.modifiedErr
	}
	return directory.Directory.SetModifiedTime(modified)
}

func (directory *outputV3PublicationDirectory) Close() error {
	return errors.Join(directory.Directory.Close(), directory.faults.closeErr)
}

type outputV3PublicationPlatform struct {
	outputcap.Platform
	root          outputcap.Directory
	guardErr      error
	guardCloseErr error
}

func (platform *outputV3PublicationPlatform) Root() outputcap.Directory { return platform.root }

func (platform *outputV3PublicationPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform.guardErr != nil {
		return nil, platform.guardErr
	}
	decorated := platform.root.(*outputV3PublicationDirectory)
	guard, err := acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return &outputV3PublicationDirectory{
				Directory: root,
				faults:    decorated.faults,
			}
		},
	)
	if err != nil || platform.guardCloseErr == nil {
		return guard, err
	}
	return &outputV3PublicationCloseFaultGuard{
		PublicOperationGuard: guard,
		closeErr:             platform.guardCloseErr,
	}, nil
}

type outputV3PublicationCloseFaultGuard struct {
	outputcap.PublicOperationGuard
	closeErr error
}

func (guard *outputV3PublicationCloseFaultGuard) Close() error {
	if guard == nil {
		return nil
	}
	var nativeErr error
	if guard.PublicOperationGuard != nil {
		nativeErr = guard.PublicOperationGuard.Close()
	}
	err := errors.Join(nativeErr, guard.closeErr)
	guard.PublicOperationGuard = nil
	return err
}

type outputV3RetirementDirectoryFaults struct {
	injected           error
	classifyErrAt      int
	absentAt           int
	classifyCalls      int
	openDirectoryErrAt int
	openDirectoryCalls int
	child              outputV3RetirementChildFaults
}

type outputV3RetirementDirectory struct {
	outputcap.Directory
	faults *outputV3RetirementDirectoryFaults
}

func (directory *outputV3RetirementDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	directory.faults.classifyCalls++
	if directory.faults.classifyCalls == directory.faults.classifyErrAt {
		return outputcap.EntryAbsent, false, directory.faults.injected
	}
	if directory.faults.classifyCalls == directory.faults.absentAt {
		return outputcap.EntryAbsent, true, nil
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *outputV3RetirementDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	directory.faults.openDirectoryCalls++
	if directory.faults.openDirectoryCalls == directory.faults.openDirectoryErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	if directory.faults.child.injected == nil {
		directory.faults.child.injected = directory.faults.injected
	}
	return &outputV3RetirementChildDirectory{
		Directory: opened,
		faults:    &directory.faults.child,
	}, nil
}

func (directory *outputV3RetirementDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3RetirementDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

type outputV3RetirementChildFaults struct {
	injected          error
	cleanupInjected   error
	observeErrAt      int
	observeOverrideAt int
	observeKind       outputcap.EntryKind
	observeCalls      int
	openFileErrAt     int
	openFileCalls     int
	removeFileErrAt   int
	removeFileCalls   int
	syncErrAt         int
	syncCalls         int
	closeErrAt        int
	closeCalls        int
	fileCloseErrAt    int
	fileCloseCalls    int
	sizeErrAt         int
	sizeAdjustmentAt  int
	sizeAdjustment    int64
	sizeCalls         int
	sameErrAt         int
	differentAt       int
	sameCalls         int
}

type outputV3RetirementChildDirectory struct {
	outputcap.Directory
	faults *outputV3RetirementChildFaults
}

func (directory *outputV3RetirementChildDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	directory.faults.observeCalls++
	if directory.faults.observeCalls == directory.faults.observeErrAt {
		return outputcap.EntryAbsent, directory.faults.injected
	}
	if directory.faults.observeCalls == directory.faults.observeOverrideAt {
		return directory.faults.observeKind, nil
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *outputV3RetirementChildDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	directory.faults.openFileCalls++
	if directory.faults.openFileCalls == directory.faults.openFileErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3RetirementFile{File: opened, faults: directory.faults}, nil
}

func (directory *outputV3RetirementChildDirectory) RemoveFile(
	name string,
	expected outputcap.File,
) error {
	directory.faults.removeFileCalls++
	if directory.faults.removeFileCalls == directory.faults.removeFileErrAt {
		return directory.faults.injected
	}
	if wrapped, ok := expected.(*outputV3RetirementFile); ok {
		expected = wrapped.File
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *outputV3RetirementChildDirectory) Sync() error {
	directory.faults.syncCalls++
	if directory.faults.syncCalls == directory.faults.syncErrAt {
		return directory.faults.injected
	}
	return directory.Directory.Sync()
}

func (directory *outputV3RetirementChildDirectory) Close() error {
	directory.faults.closeCalls++
	closeErr := directory.Directory.Close()
	if directory.faults.closeCalls == directory.faults.closeErrAt {
		return errors.Join(closeErr, directory.faults.cleanupError())
	}
	return closeErr
}

type outputV3RetirementFile struct {
	outputcap.File
	faults *outputV3RetirementChildFaults
}

func (file *outputV3RetirementFile) Size() (uint64, error) {
	file.faults.sizeCalls++
	if file.faults.sizeCalls == file.faults.sizeErrAt {
		return 0, file.faults.injected
	}
	size, err := file.File.Size()
	if err != nil || file.faults.sizeCalls != file.faults.sizeAdjustmentAt {
		return size, err
	}
	if file.faults.sizeAdjustment > 0 {
		return size + uint64(file.faults.sizeAdjustment), nil
	}
	return size - uint64(-file.faults.sizeAdjustment), nil
}

func (file *outputV3RetirementFile) SameFile(other outputcap.File) (bool, error) {
	file.faults.sameCalls++
	if file.faults.sameCalls == file.faults.sameErrAt {
		return false, file.faults.injected
	}
	if file.faults.sameCalls == file.faults.differentAt {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3RetirementFile); ok {
		other = wrapped.File
	}
	return file.File.SameFile(other)
}

func (file *outputV3RetirementFile) Close() error {
	file.faults.fileCloseCalls++
	closeErr := file.File.Close()
	if file.faults.fileCloseCalls == file.faults.fileCloseErrAt {
		return errors.Join(closeErr, file.faults.cleanupError())
	}
	return closeErr
}

func (faults *outputV3RetirementChildFaults) cleanupError() error {
	if faults.cleanupInjected != nil {
		return faults.cleanupInjected
	}
	return faults.injected
}

type outputV3RetirementRecordFaults struct {
	injected        error
	cleanupInjected error
	openFileErrAt   int
	openFileCalls   int
	readErrAt       int
	readCalls       int
	fileCloseErrAt  int
	fileCloseCalls  int
	removeFileErrAt int
	removeFileCalls int
	syncErrAt       int
	syncCalls       int
}

type outputV3RetirementRecordDirectory struct {
	outputcap.Directory
	faults *outputV3RetirementRecordFaults
}

func (directory *outputV3RetirementRecordDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	directory.faults.openFileCalls++
	if directory.faults.openFileCalls == directory.faults.openFileErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3RetirementRecordFile{File: opened, faults: directory.faults}, nil
}

func (directory *outputV3RetirementRecordDirectory) RemoveFile(
	name string,
	expected outputcap.File,
) error {
	directory.faults.removeFileCalls++
	if directory.faults.removeFileCalls == directory.faults.removeFileErrAt {
		return directory.faults.injected
	}
	if wrapped, ok := expected.(*outputV3RetirementRecordFile); ok {
		expected = wrapped.File
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *outputV3RetirementRecordDirectory) Sync() error {
	directory.faults.syncCalls++
	if directory.faults.syncCalls == directory.faults.syncErrAt {
		return directory.faults.injected
	}
	return directory.Directory.Sync()
}

type outputV3RetirementRecordFile struct {
	outputcap.File
	faults *outputV3RetirementRecordFaults
}

func (file *outputV3RetirementRecordFile) Close() error {
	file.faults.fileCloseCalls++
	closeErr := file.File.Close()
	if file.faults.fileCloseCalls == file.faults.fileCloseErrAt {
		return errors.Join(closeErr, file.faults.cleanupError())
	}
	return closeErr
}

func (faults *outputV3RetirementRecordFaults) cleanupError() error {
	if faults.cleanupInjected != nil {
		return faults.cleanupInjected
	}
	return faults.injected
}

func (file *outputV3RetirementRecordFile) ReadAt(data []byte, offset int64) (int, error) {
	file.faults.readCalls++
	if file.faults.readCalls == file.faults.readErrAt {
		return 0, file.faults.injected
	}
	return file.File.ReadAt(data, offset)
}

func outputV3PreparedRetirement(
	t *testing.T,
) (
	*Session,
	outputcap.Directory,
	string,
	resumestate.BoundFileRecord,
	transfer.OutputFileBinding,
) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	record := v3RecoveryPrepareRetiringCut(t, opened.Session, file)
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	recordDir := outputV3SemanticOpenShard(t, opened.Session.filesDir, recordName.Shard(), false)
	t.Cleanup(func() {
		if err := recordDir.Close(); err != nil {
			t.Errorf("close retirement record shard: %v", err)
		}
	})
	retiring, err := resumestate.BindFileRecord(
		opened.Session.stateSnapshot(), recordName.Shard(), recordName.Name(), record,
	)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := outputBindingForRecord(opened.Session.SessionID(), file.Descriptor, record)
	if err != nil {
		t.Fatal(err)
	}
	return opened.Session, recordDir, recordName.Name(), retiring, binding
}

func outputV3RetireBoundFileAsOperation(
	t *testing.T,
	session *Session,
	recordDir outputcap.Directory,
	recordName string,
	retiring resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
) (transfer.FileSettlement, bool, error) {
	t.Helper()
	if err := session.beginOperation(); err != nil {
		t.Fatalf("begin retirement operation: %v", err)
	}
	defer session.endOperation()
	return session.retireBoundFile(recordDir, recordName, retiring, binding)
}

func closeOutputV3ObservedFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func outputV3PersistedFileRecord(
	t *testing.T,
	session *Session,
	locator string,
) resumestate.FileRecord {
	t.Helper()
	name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(locator))
	shard := outputV3SemanticOpenShard(t, session.filesDir, name.Shard(), false)
	encoded, readErr := outputnamespace.ReadRecord(shard, name.Name(), resumestate.MaxFileStateBytes)
	closeErr := shard.Close()
	record, decodeErr := resumestate.DecodeFileRecord(encoded)
	if err := errors.Join(readErr, closeErr, decodeErr); err != nil {
		t.Fatalf("read persisted file record: %v", err)
	}
	return record
}

func outputV3PublicationReadyTransaction(
	t *testing.T,
) (*Session, *FileTransaction) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
	t.Cleanup(func() {
		transaction.mu.Lock()
		open := transaction.lifecycle == FileTransactionOpen
		transaction.mu.Unlock()
		if open {
			outputV3SemanticDetachTransaction(t, opened.Session, transaction)
		}
	})
	if err := transaction.data.SetModifiedTime(transaction.descriptor.ModifiedTime()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.Sync(); err != nil {
		t.Fatal(err)
	}
	return opened.Session, transaction
}

func outputV3CreatePublicCollision(
	t *testing.T,
	session *Session,
	locator string,
	kind outputcap.EntryKind,
) {
	t.Helper()
	parentPath, leaf, err := outputLocatorParentAndLeaf(locator)
	if err != nil {
		t.Fatal(err)
	}
	requirement := outputAncestryRequirement{path: parentPath, authority: outputAncestryCreateAuthority}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cleanupErr := errors.Join(validation.Revalidate(requirement), closeOutputAncestryValidation(validation)); cleanupErr != nil {
			t.Errorf("finish public collision setup: %v", cleanupErr)
		}
	}()
	parent, err := validation.directory(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validation.revalidateRetainedDirectory(parentPath, outputAncestryCreateAuthority); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case outputcap.EntryRegularFile:
		file, createErr := parent.CreateFile(leaf, false, 1)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if err := errors.Join(file.Sync(), file.Close(), parent.Sync()); err != nil {
			t.Fatal(err)
		}
	case outputcap.EntryDirectory:
		directory, createErr := parent.CreateDirectory(leaf, false)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if err := errors.Join(directory.Sync(), directory.Close(), parent.Sync()); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported collision kind %d", kind)
	}
}
