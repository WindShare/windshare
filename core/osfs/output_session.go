package osfs

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const MaxFilesystemOutputTransactions = 32

var (
	ErrOutputJournalCorrupt    = errors.New("osfs: output journal is corrupt")
	ErrOutputBinding           = errors.New("osfs: output journal binding does not match this session")
	ErrOutputSessionActive     = errors.New("osfs: output session is already active")
	ErrOutputSessionClosed     = errors.New("osfs: output session is closed")
	ErrOutputFileActive        = errors.New("osfs: output file transaction is already active")
	ErrOutputTransactionLimit  = errors.New("osfs: output transaction limit reached")
	ErrUnsupportedOutputVolume = errors.New("osfs: output volume does not support recoverable atomic publication")
)

type FilesystemOutputSessionConfig struct {
	RootPath      string
	ShareInstance catalog.ShareInstance
	SessionID     transfer.OutputSessionID
	ResumeIntent  OutputResumeIntent
}

type FilesystemOutputSession struct {
	rootPath     string
	root         *os.Root
	share        catalog.ShareInstance
	session      transfer.OutputSessionID
	intent       OutputResumeIntent
	rootBinding  outputRootBinding
	capabilities transfer.OutputCapabilities
	journalName  string
	lock         *outputSessionLock
	hook         checkpointHook

	mu                 sync.Mutex
	journal            outputJournalDocument
	active             map[string]*filesystemFileTransaction
	createdDirectories []string
	closed             bool
}

func NewFilesystemOutputSession(config FilesystemOutputSessionConfig) (*FilesystemOutputSession, error) {
	return newFilesystemOutputSession(config, nil)
}

func newFilesystemOutputSession(config FilesystemOutputSessionConfig, hook checkpointHook) (*FilesystemOutputSession, error) {
	return newFilesystemOutputSessionExpected(config, hook, nil)
}

func newFilesystemOutputSessionExpected(config FilesystemOutputSessionConfig, hook checkpointHook, expectedRoot *outputRootBinding) (*FilesystemOutputSession, error) {
	if config.RootPath == "" || config.ShareInstance.IsZero() || config.SessionID.IsZero() || config.ResumeIntent.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	abs, err := filepath.Abs(config.RootPath)
	if err != nil {
		return nil, filesystemPathFailure("resolve v2 output root", config.RootPath, err)
	}
	if exceedsPathLimit(abs) {
		return nil, filesystemPathFailure("resolve v2 output root", config.RootPath, ErrPathTooLong)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, filesystemPathFailure("create v2 output root", abs, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, filesystemPathFailure("open v2 output root", abs, err)
	}
	rootBinding, err := bindOutputRoot(abs, root)
	if err != nil {
		_ = root.Close()
		return nil, filesystemPathFailure("bind v2 output root", abs, err)
	}
	if expectedRoot != nil && rootBinding != *expectedRoot {
		_ = root.Close()
		return nil, ErrOutputBinding
	}
	durability, modifiedTime := outputPlatformCapabilities()
	capabilities, err := transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Durability: durability, Mode: transfer.OutputNativeTree, RandomWrite: true,
		FileFailureIsolation: true, ModifiedTime: modifiedTime,
	})
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	journalName := outputJournalPrefix + encodeOutputFilenameToken(config.SessionID.Bytes()) + ".journal"
	lockName := outputSessionLockName(config.SessionID)
	lock, err := acquireOutputSessionLock(root, lockName)
	if err != nil {
		_ = root.Close()
		return nil, filesystemPathFailure("lock v2 output session", filepath.Join(abs, lockName), err)
	}
	document := newOutputJournal(filesystemOutputBackendID, config.SessionID, config.ShareInstance, config.ResumeIntent, rootBinding)
	loaded, loadErr := readOutputJournalAt(root, journalName)
	switch {
	case loadErr == nil:
		if loaded.Backend != string(filesystemOutputBackendID) ||
			loaded.OutputSession != encodeOutputBytes(config.SessionID.Bytes()) ||
			loaded.ShareInstance != encodeOutputBytes(config.ShareInstance.Bytes()) ||
			loaded.ResumeIntent != encodeOutputBytes(config.ResumeIntent.Bytes()) ||
			loaded.RootLocator != rootBinding.locator || loaded.RootIdentity != rootBinding.identity {
			_ = lock.close(true)
			_ = root.Close()
			return nil, ErrOutputBinding
		}
		document = loaded
	case !errors.Is(loadErr, os.ErrNotExist):
		_ = lock.close(true)
		_ = root.Close()
		return nil, loadErr
	default:
		document, err = persistOutputJournal(root, journalName, document, hook)
		if err != nil {
			_ = lock.close(true)
			_ = root.Close()
			return nil, err
		}
	}
	return &FilesystemOutputSession{
		rootPath: abs, root: root, share: config.ShareInstance, session: config.SessionID,
		intent: config.ResumeIntent, rootBinding: rootBinding,
		capabilities: capabilities, journalName: journalName, lock: lock, hook: hook, journal: document,
		active: make(map[string]*filesystemFileTransaction),
	}, nil
}

func (s *FilesystemOutputSession) BackendID() transfer.OutputBackendID {
	return filesystemOutputBackendID
}
func (s *FilesystemOutputSession) SessionID() transfer.OutputSessionID { return s.session }
func (s *FilesystemOutputSession) ResumeIntent() OutputResumeIntent    { return s.intent }
func (s *FilesystemOutputSession) Capabilities() transfer.OutputCapabilities {
	return s.capabilities
}

func (s *FilesystemOutputSession) EnsureDirectory(ctx context.Context, directory transfer.OutputDirectory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, relative, err := s.resolve(directory.Path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.NewOutputSessionError(ErrOutputSessionClosed, true)
	}
	components := strings.Split(relative, string(filepath.Separator))
	for index := range components {
		prefix := filepath.Join(components[:index+1]...)
		info, statErr := s.root.Lstat(prefix)
		if statErr == nil {
			if !info.IsDir() || isReparsePoint(info) {
				return filesystemPathFailure("validate output directory", filepath.Join(s.rootPath, prefix), ErrPathEscape)
			}
			continue
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return filesystemPathFailure("inspect output directory", filepath.Join(s.rootPath, prefix), statErr)
		}
		if err := s.root.Mkdir(prefix, dirPerm); err != nil {
			return filesystemPathFailure("create output directory", filepath.Join(s.rootPath, prefix), err)
		}
		s.createdDirectories = append(s.createdDirectories, filepath.ToSlash(prefix))
	}
	_ = canonical
	return nil
}

func (s *FilesystemOutputSession) FinalizeDirectory(ctx context.Context, directory transfer.OutputDirectory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.capabilities.ModifiedTime || !directory.ModifiedTime.Present() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return transfer.NewOutputSessionError(ErrOutputSessionClosed, true)
		}
		return nil
	}
	_, relative, err := s.resolve(directory.Path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.NewOutputSessionError(ErrOutputSessionClosed, true)
	}
	info, err := s.root.Lstat(relative)
	if err != nil || !info.IsDir() || isReparsePoint(info) {
		return transfer.NewOutputSessionError(filesystemPathFailure("validate finalized directory", directory.Path, errors.Join(err, ErrPathEscape)), false)
	}
	if err := s.root.Chtimes(relative, time.Time{}, catalogTime(directory.ModifiedTime)); err != nil {
		return transfer.NewOutputSessionError(filesystemPathFailure("set directory modified time", directory.Path, err), false)
	}
	after, err := s.root.Lstat(relative)
	if err != nil || !after.IsDir() || isReparsePoint(after) || !os.SameFile(info, after) {
		return transfer.NewOutputSessionError(filesystemPathFailure("verify finalized directory", directory.Path, errors.Join(err, ErrPathEscape)), false)
	}
	return nil
}

func (s *FilesystemOutputSession) BeginFile(ctx context.Context, output transfer.OutputFile) (transfer.FileTransaction, transfer.VerifiedDurableRanges, error) {
	if err := ctx.Err(); err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	canonical, relative, err := s.resolve(output.Path)
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	descriptor := output.Descriptor
	if descriptor.ShareInstance() != s.share || descriptor.ExactSize() != output.ExpectedSize || output.ExpectedSize > math.MaxInt64 {
		return nil, transfer.VerifiedDurableRanges{}, transfer.ErrInvalidOutputBinding
	}
	if parent := filepath.Dir(canonical); parent != "." {
		if err := s.EnsureDirectory(ctx, transfer.OutputDirectory{Path: filepath.ToSlash(parent)}); err != nil {
			return nil, transfer.VerifiedDurableRanges{}, err
		}
	}
	locator, _ := transfer.NewPathOutputLocator(canonical)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, transfer.VerifiedDurableRanges{}, transfer.NewOutputSessionError(ErrOutputSessionClosed, true)
	}
	if s.active[canonical] != nil {
		return nil, transfer.VerifiedDurableRanges{}, ErrOutputFileActive
	}
	if len(s.active) >= MaxFilesystemOutputTransactions {
		return nil, transfer.VerifiedDurableRanges{}, ErrOutputTransactionLimit
	}
	if existing, found := s.journal.file(canonical); found {
		transaction, durable, reused, reuseErr := s.resumeFileLocked(ctx, output, locator, relative, *existing)
		if reuseErr != nil {
			return nil, transfer.VerifiedDurableRanges{}, reuseErr
		}
		if reused {
			return transaction, durable, nil
		}
	}
	return s.beginNewFileLocked(ctx, output, locator, relative)
}

func (s *FilesystemOutputSession) resumeFileLocked(
	ctx context.Context,
	output transfer.OutputFile,
	locator transfer.OutputLocator,
	relative string,
	entry outputJournalFile,
) (*filesystemFileTransaction, transfer.VerifiedDurableRanges, bool, error) {
	identity, binding, err := s.resumeOutputBinding(output, locator, entry)
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, false, err
	}
	if !entry.matches(binding) {
		if err := s.invalidateJournalFileLocked(entry); err != nil {
			return nil, transfer.VerifiedDurableRanges{}, false, err
		}
		return nil, transfer.VerifiedDurableRanges{}, false, nil
	}
	ranges, err := entry.ranges()
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, false, ErrOutputJournalCorrupt
	}
	openPath, published, recoverable := s.resumeOpenPath(relative, entry, identity, ranges)
	if !recoverable {
		if err := s.invalidateJournalFileLocked(entry); err != nil {
			return nil, transfer.VerifiedDurableRanges{}, false, err
		}
		return nil, transfer.VerifiedDurableRanges{}, false, nil
	}
	file, err := s.openVerifiedOutput(openPath, identity, entry.ExactSize)
	if err != nil {
		if err := s.invalidateJournalFileLocked(entry); err != nil {
			return nil, transfer.VerifiedDurableRanges{}, false, err
		}
		return nil, transfer.VerifiedDurableRanges{}, false, nil
	}
	if published {
		if err := s.recoverPublishedOutputLocked(output, relative, &entry, identity, file); err != nil {
			_ = file.Close()
			return nil, transfer.VerifiedDurableRanges{}, false, err
		}
	}
	transaction := &filesystemFileTransaction{
		session: s, file: file, binding: binding, stage: entry.Stage, modified: output.Descriptor.ModifiedTime(),
		durable: ranges, generation: entry.Generation, published: published,
	}
	s.active[entry.Path] = transaction
	verified, _ := transfer.VerifyDurableRanges(binding, entry.Generation, ranges)
	return transaction, verified, true, nil
}

func (s *FilesystemOutputSession) recoverPublishedOutputLocked(output transfer.OutputFile, relative string, entry *outputJournalFile, identity transfer.OutputObjectIdentity, file *os.File) error {
	if !entry.Published && entry.Generation == math.MaxUint64 {
		return transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
	}
	if err := s.removeOwnedPath(entry.Stage, identity, entry.ExactSize); err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	if entry.Published {
		return nil
	}
	if err := s.restorePublishedOutputMetadata(output, relative, identity, file); err != nil {
		return err
	}
	entry.Published = true
	entry.Generation++
	candidate := s.journal.clone()
	candidate.put(*entry)
	loaded, err := persistOutputJournal(s.root, s.journalName, candidate, s.hook)
	if err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	s.journal = loaded
	return nil
}

func (s *FilesystemOutputSession) restorePublishedOutputMetadata(output transfer.OutputFile, relative string, identity transfer.OutputObjectIdentity, file *os.File) error {
	modified := output.Descriptor.ModifiedTime()
	if s.capabilities.ModifiedTime && modified.Present() {
		if err := s.root.Chtimes(relative, time.Time{}, catalogTime(modified)); err != nil {
			return transfer.NewOutputSessionError(filesystemPathFailure("restore output modified time", output.Path, err), true)
		}
	}
	if err := errors.Join(file.Sync(), s.verifyFile(file, identity, output.ExpectedSize)); err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	return nil
}

func (s *FilesystemOutputSession) beginNewFileLocked(
	ctx context.Context,
	output transfer.OutputFile,
	locator transfer.OutputLocator,
	relative string,
) (*filesystemFileTransaction, transfer.VerifiedDurableRanges, error) {
	if _, err := s.root.Lstat(relative); err == nil {
		return nil, transfer.VerifiedDurableRanges{}, categorizedPathFailure("open output file", output.Path, ErrAlreadyExists, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	stage, err := newOutputStageName()
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	file, err := s.root.OpenFile(stage, os.O_RDWR|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = file.Close()
			_ = s.root.Remove(stage)
		}
	}()
	if err := file.Truncate(int64(output.ExpectedSize)); err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	if err := file.Sync(); err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	identity, err := outputObjectIdentity(file)
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	binding, err := transfer.NewOutputFileBinding(filesystemOutputBackendID, s.session, output.Descriptor, locator, identity)
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, err
	}
	empty, _ := content.NewRangeSet(nil)
	entry := journalFileFromBinding(binding, stage, 0, empty, false)
	candidate := s.journal.clone()
	candidate.put(entry)
	loaded, err := persistOutputJournal(s.root, s.journalName, candidate, s.hook)
	if err != nil {
		return nil, transfer.VerifiedDurableRanges{}, transfer.NewOutputSessionError(err, true)
	}
	if err := s.verifyOwnedPath(stage, identity, output.ExpectedSize); err != nil {
		return nil, transfer.VerifiedDurableRanges{}, transfer.NewOutputSessionError(err, true)
	}
	s.journal = loaded
	transaction := &filesystemFileTransaction{
		session: s, file: file, binding: binding, stage: stage, modified: output.Descriptor.ModifiedTime(), durable: empty,
	}
	s.active[locator.CanonicalPath()] = transaction
	cleanup = false
	verified, _ := transfer.VerifyDurableRanges(binding, 0, empty)
	_ = ctx
	return transaction, verified, nil
}

func (s *FilesystemOutputSession) resolve(path string) (string, string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil {
		return "", "", err
	}
	relative := filepath.FromSlash(canonical)
	if !filepath.IsLocal(relative) || exceedsPathLimit(filepath.Join(s.rootPath, relative)) {
		return "", "", ErrPathEscape
	}
	return canonical, relative, nil
}

func (s *FilesystemOutputSession) verifyFile(file *os.File, identity transfer.OutputObjectIdentity, size uint64) error {
	if file == nil {
		return ErrOwnedFileMissing
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || isReparsePoint(info) || info.Size() < 0 || uint64(info.Size()) != size {
		return errors.Join(ErrOwnedFileMissing, err)
	}
	current, err := outputObjectIdentity(file)
	if err != nil || current != identity {
		return errors.Join(ErrOwnedFileMissing, err)
	}
	return nil
}

func (s *FilesystemOutputSession) verifyOwnedPath(path string, identity transfer.OutputObjectIdentity, size uint64) error {
	file, err := s.root.Open(path)
	if err != nil {
		return err
	}
	verifyErr := s.verifyFile(file, identity, size)
	return errors.Join(verifyErr, file.Close())
}

func (s *FilesystemOutputSession) invalidateJournalFileLocked(entry outputJournalFile) error {
	identityBytes, _ := decodeOutputBytes(entry.ObjectIdentity, transfer.OutputObjectIdentityBytes)
	identity, _ := transfer.OutputObjectIdentityFromBytes(identityBytes)
	var result error
	for _, path := range []string{entry.Stage, filepath.FromSlash(entry.Path)} {
		result = errors.Join(result, s.removeOwnedPath(path, identity, entry.ExactSize))
	}
	if result != nil {
		return transfer.NewOutputSessionError(result, true)
	}
	candidate := s.journal.clone()
	candidate.remove(entry.Path)
	loaded, err := persistOutputJournal(s.root, s.journalName, candidate, s.hook)
	if err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	s.journal = loaded
	return nil
}

func (s *FilesystemOutputSession) publishFile(transaction *filesystemFileTransaction) error {
	path := transaction.binding.Locator().CanonicalPath()
	relative := filepath.FromSlash(path)
	if err := publishOutputFile(s.root, transaction.stage, relative); err != nil {
		return err
	}
	if s.capabilities.ModifiedTime && transaction.modified.Present() {
		if err := s.root.Chtimes(relative, time.Time{}, catalogTime(transaction.modified)); err != nil {
			return err
		}
	}
	file, err := s.root.OpenFile(relative, os.O_RDWR, filePerm)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	verifyErr := s.verifyFile(file, transaction.binding.ObjectIdentity(), transaction.binding.ExactSize())
	closeErr := file.Close()
	if verifyErr != nil || closeErr != nil {
		return errors.Join(verifyErr, closeErr)
	}
	if transaction.generation == math.MaxUint64 {
		return transfer.ErrOutputContract
	}
	return s.persistCheckpoint(transaction.binding, transaction.stage, transaction.generation+1, transaction.durable, true)
}

func (s *FilesystemOutputSession) abortFile(binding transfer.OutputFileBinding, stage string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := binding.Locator().CanonicalPath()
	current, found := s.journal.file(path)
	if !found || !current.matches(binding) || current.Stage != stage {
		delete(s.active, path)
		return nil
	}
	var cleanupErr error
	if !current.Published {
		for _, ownedPath := range []string{stage, filepath.FromSlash(path)} {
			cleanupErr = errors.Join(cleanupErr, s.removeOwnedPath(ownedPath, binding.ObjectIdentity(), binding.ExactSize()))
		}
	}
	if cleanupErr != nil {
		delete(s.active, path)
		return transfer.NewOutputSessionError(cleanupErr, true)
	}
	candidate := s.journal.clone()
	candidate.remove(path)
	loaded, err := persistOutputJournal(s.root, s.journalName, candidate, s.hook)
	if err == nil {
		s.journal = loaded
	}
	delete(s.active, path)
	if err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	return nil
}

func (s *FilesystemOutputSession) completeFile(path string, transaction *filesystemFileTransaction) {
	s.mu.Lock()
	if s.active[path] == transaction {
		delete(s.active, path)
	}
	s.mu.Unlock()
}

func (s *FilesystemOutputSession) FinishJob(ctx context.Context, outcome transfer.JobOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.NewOutputSessionError(ErrOutputSessionClosed, true)
	}
	if outcome != transfer.JobSucceeded && outcome != transfer.JobCompletedWithErrors {
		return transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
	}
	if len(s.active) != 0 {
		return transfer.NewOutputSessionError(transfer.ErrIncompleteOutputFile, true)
	}
	for _, file := range s.journal.Files {
		if !file.Published {
			return transfer.NewOutputSessionError(transfer.ErrIncompleteOutputFile, true)
		}
	}
	err := removeOutputJournal(s.root, s.journalName)
	s.closed = true
	return errors.Join(err, s.lock.close(true), s.root.Close())
}

func (s *FilesystemOutputSession) AbortJob(ctx context.Context, cause error) error {
	_ = cause
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	// Claim shutdown before releasing the lock so no new transaction can enter
	// while existing file transactions are being unwound outside the session lock.
	s.closed = true
	transactions := make([]*filesystemFileTransaction, 0, len(s.active))
	for _, transaction := range s.active {
		transactions = append(transactions, transaction)
	}
	s.mu.Unlock()
	var result error
	for _, transaction := range transactions {
		_, err := transaction.Abort(context.WithoutCancel(ctx), cause)
		result = errors.Join(result, err)
	}
	s.mu.Lock()
	var cleanupErr error
	for _, entry := range s.journal.Files {
		if entry.Published {
			continue
		}
		identityBytes, _ := decodeOutputBytes(entry.ObjectIdentity, transfer.OutputObjectIdentityBytes)
		identity, _ := transfer.OutputObjectIdentityFromBytes(identityBytes)
		for _, ownedPath := range []string{entry.Stage, filepath.FromSlash(entry.Path)} {
			cleanupErr = errors.Join(cleanupErr, s.removeOwnedPath(ownedPath, identity, entry.ExactSize))
		}
	}
	result = errors.Join(result, cleanupErr)
	if cleanupErr == nil {
		result = errors.Join(result, removeOutputJournal(s.root, s.journalName))
	}
	for index := len(s.createdDirectories) - 1; index >= 0; index-- {
		if err := s.root.Remove(filepath.FromSlash(s.createdDirectories[index])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Non-empty directories can contain a completed sibling and must stay.
			if !isDirectoryNotEmptyError(err) {
				result = errors.Join(result, err)
			}
		}
	}
	result = errors.Join(result, s.lock.close(true), s.root.Close())
	s.mu.Unlock()
	return result
}

func catalogTime(modified catalog.ModifiedTime) time.Time {
	return time.Unix(modified.Seconds(), int64(modified.Nanoseconds()))
}
