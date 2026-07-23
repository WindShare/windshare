package osfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const (
	OutputResumeIntentBytes    = sha256.Size
	MaxOutputJournalCandidates = 256
	outputIntentLockPrefix     = ".wsresume-output-intent-"
	outputQuarantinePrefix     = ".wsresume-output-quarantine-"
	outputDiscoveryBatchSize   = 128
)

var (
	ErrOutputDiscoveryAmbiguous = errors.New("osfs: more than one output session matches the resume intent")
	ErrOutputDiscoveryLimit     = errors.New("osfs: output journal discovery limit reached")
	ErrOutputDiscoveryUnsafe    = errors.New("osfs: output journal cannot be safely classified")
)

type OutputResumeIntent [OutputResumeIntentBytes]byte

func OutputResumeIntentFromBytes(raw []byte) (OutputResumeIntent, error) {
	if len(raw) != OutputResumeIntentBytes {
		return OutputResumeIntent{}, transfer.ErrInvalidOutputBinding
	}
	var intent OutputResumeIntent
	copy(intent[:], raw)
	if intent.IsZero() {
		return OutputResumeIntent{}, transfer.ErrInvalidOutputBinding
	}
	return intent, nil
}

func (i OutputResumeIntent) Bytes() []byte { return append([]byte(nil), i[:]...) }
func (i OutputResumeIntent) IsZero() bool  { return i == OutputResumeIntent{} }

// FilesystemOutputIntent is the stable, caller-owned job identity. The random
// OutputSessionID is deliberately absent: discovery obtains it only from a
// validated journal bound to this share, intent, backend, and retained root.
type FilesystemOutputIntent struct {
	RootPath      string
	ShareInstance catalog.ShareInstance
	ResumeIntent  OutputResumeIntent
}

type OutputSessionIDGenerator interface {
	NewOutputSessionID() (transfer.OutputSessionID, error)
}

type OutputSessionIDGeneratorFunc func() (transfer.OutputSessionID, error)

func (f OutputSessionIDGeneratorFunc) NewOutputSessionID() (transfer.OutputSessionID, error) {
	return f()
}

type FilesystemOutputAuthorityConfig struct {
	SessionIDs OutputSessionIDGenerator
}

type FilesystemOutputAuthority struct {
	ids OutputSessionIDGenerator
}

type FilesystemOutputOpen struct {
	Session     *FilesystemOutputSession
	Reopened    bool
	Quarantined int
}

func NewFilesystemOutputAuthority(config FilesystemOutputAuthorityConfig) (*FilesystemOutputAuthority, error) {
	ids := config.SessionIDs
	if ids == nil {
		ids = cryptographicOutputSessionIDs{}
	}
	return &FilesystemOutputAuthority{ids: ids}, nil
}

type cryptographicOutputSessionIDs struct{}

func (cryptographicOutputSessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	var raw [transfer.OutputSessionIdentityBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return transfer.OutputSessionID{}, err
	}
	return transfer.OutputSessionIDFromBytes(raw[:])
}

func (a *FilesystemOutputAuthority) OpenOrCreate(ctx context.Context, intent FilesystemOutputIntent) (FilesystemOutputOpen, error) {
	if a == nil || a.ids == nil || intent.RootPath == "" || intent.ShareInstance.IsZero() || intent.ResumeIntent.IsZero() {
		return FilesystemOutputOpen{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return FilesystemOutputOpen{}, err
	}
	abs, root, binding, err := openOutputDiscoveryRoot(intent.RootPath)
	if err != nil {
		return FilesystemOutputOpen{}, err
	}
	rootOwned := true
	defer func() {
		if rootOwned {
			_ = root.Close()
		}
	}()
	intentLockName := outputIntentLockName(intent.ShareInstance, intent.ResumeIntent)
	intentLock, err := acquireOutputSessionLock(root, intentLockName)
	if err != nil {
		return FilesystemOutputOpen{}, filesystemPathFailure("lock output resume intent", filepath.Join(abs, intentLockName), err)
	}
	lockOwned := true
	defer func() {
		if lockOwned {
			_ = intentLock.close(true)
		}
	}()

	matching, quarantined, err := discoverMatchingOutputSessions(ctx, root, intent, binding)
	if err != nil {
		return FilesystemOutputOpen{}, err
	}
	if len(matching) > 1 {
		return FilesystemOutputOpen{}, ErrOutputDiscoveryAmbiguous
	}
	var session *FilesystemOutputSession
	reopened := len(matching) == 1
	if len(matching) == 1 {
		session, err = openDiscoveredOutputSession(abs, intent, binding, matching[0])
	} else {
		session, err = a.createOutputSession(abs, intent, binding)
	}
	if err != nil {
		return FilesystemOutputOpen{}, err
	}
	closeErr := errors.Join(intentLock.close(true), root.Close())
	lockOwned, rootOwned = false, false
	if closeErr != nil {
		abandonFilesystemOutputSession(session)
		return FilesystemOutputOpen{}, closeErr
	}
	return FilesystemOutputOpen{Session: session, Reopened: reopened, Quarantined: quarantined}, nil
}

func discoverMatchingOutputSessions(ctx context.Context, root *os.Root, intent FilesystemOutputIntent, binding outputRootBinding) ([]transfer.OutputSessionID, int, error) {
	names, err := discoverOutputJournalNames(root)
	if err != nil {
		return nil, 0, err
	}
	matching := make([]transfer.OutputSessionID, 0, 1)
	quarantined := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, quarantined, err
		}
		session, match, quarantinedName, err := inspectOutputJournal(root, name, intent, binding)
		if err != nil {
			return nil, quarantined, err
		}
		if quarantinedName {
			quarantined++
		}
		if match {
			matching = append(matching, session)
		}
	}
	return matching, quarantined, nil
}

func inspectOutputJournal(root *os.Root, name string, intent FilesystemOutputIntent, binding outputRootBinding) (transfer.OutputSessionID, bool, bool, error) {
	session, nameOK := outputSessionIDFromJournalName(name)
	if !nameOK {
		if err := quarantineOutputJournal(root, name); err != nil {
			return transfer.OutputSessionID{}, false, false, errors.Join(ErrOutputDiscoveryUnsafe, err)
		}
		return transfer.OutputSessionID{}, false, true, nil
	}
	document, err := readOutputJournalAt(root, name)
	if err != nil {
		if !errors.Is(err, ErrOutputJournalCorrupt) {
			return transfer.OutputSessionID{}, false, false, errors.Join(ErrOutputDiscoveryUnsafe, err)
		}
		if err := quarantineInactiveOutputJournal(root, name, session); err != nil {
			return transfer.OutputSessionID{}, false, false, err
		}
		return transfer.OutputSessionID{}, false, true, nil
	}
	switch classifyOutputJournal(document, session, intent, binding) {
	case outputJournalForeign:
		return transfer.OutputSessionID{}, false, false, nil
	case outputJournalStale:
		if err := quarantineInactiveOutputJournal(root, name, session); err != nil {
			return transfer.OutputSessionID{}, false, false, err
		}
		return transfer.OutputSessionID{}, false, true, nil
	case outputJournalMatch:
		return session, true, false, nil
	default:
		return transfer.OutputSessionID{}, false, false, ErrOutputDiscoveryUnsafe
	}
}

func openDiscoveredOutputSession(rootPath string, intent FilesystemOutputIntent, binding outputRootBinding, session transfer.OutputSessionID) (*FilesystemOutputSession, error) {
	config := FilesystemOutputSessionConfig{
		RootPath: rootPath, ShareInstance: intent.ShareInstance, SessionID: session, ResumeIntent: intent.ResumeIntent,
	}
	return newFilesystemOutputSessionExpected(config, nil, &binding)
}

func (a *FilesystemOutputAuthority) createOutputSession(rootPath string, intent FilesystemOutputIntent, binding outputRootBinding) (*FilesystemOutputSession, error) {
	for range outputNamespaceAllocationAttempts {
		session, err := a.ids.NewOutputSessionID()
		if err != nil {
			return nil, err
		}
		if session.IsZero() {
			continue
		}
		opened, err := openDiscoveredOutputSession(rootPath, intent, binding, session)
		if errors.Is(err, ErrOutputBinding) || errors.Is(err, ErrOutputSessionActive) {
			continue
		}
		return opened, err
	}
	return nil, errors.New("osfs: output session identity generator did not produce an unused non-zero identity")
}

func openOutputDiscoveryRoot(rootPath string) (string, *os.Root, outputRootBinding, error) {
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return "", nil, outputRootBinding{}, filesystemPathFailure("resolve output discovery root", rootPath, err)
	}
	if exceedsPathLimit(abs) {
		return "", nil, outputRootBinding{}, filesystemPathFailure("resolve output discovery root", rootPath, ErrPathTooLong)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return "", nil, outputRootBinding{}, filesystemPathFailure("create output discovery root", abs, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return "", nil, outputRootBinding{}, filesystemPathFailure("open output discovery root", abs, err)
	}
	binding, err := bindOutputRoot(abs, root)
	if err != nil {
		return "", nil, outputRootBinding{}, errors.Join(filesystemPathFailure("bind output discovery root", abs, err), root.Close())
	}
	return abs, root, binding, nil
}

func discoverOutputJournalNames(root *os.Root) ([]string, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	names := make([]string, 0)
	for {
		entries, readErr := directory.ReadDir(outputDiscoveryBatchSize)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, outputJournalPrefix) || !strings.HasSuffix(name, ".journal") ||
				strings.HasPrefix(name, outputQuarantinePrefix) {
				continue
			}
			names = append(names, name)
			if len(names) > MaxOutputJournalCandidates {
				return nil, ErrOutputDiscoveryLimit
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	slices.Sort(names)
	return names, nil
}

func readOutputJournalAt(root *os.Root, name string) (outputJournalDocument, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return outputJournalDocument{}, err
	}
	if !before.Mode().IsRegular() || isReparsePoint(before) {
		return outputJournalDocument{}, ErrOutputJournalCorrupt
	}
	file, err := root.Open(name)
	if err != nil {
		return outputJournalDocument{}, err
	}
	after, statErr := file.Stat()
	if statErr != nil || !after.Mode().IsRegular() || isReparsePoint(after) || !os.SameFile(before, after) {
		return outputJournalDocument{}, errors.Join(ErrOutputJournalCorrupt, statErr, file.Close())
	}
	return decodeOutputJournalFile(file)
}

func decodeOutputJournalFile(file *os.File) (outputJournalDocument, error) {
	encoded, readErr := io.ReadAll(io.LimitReader(file, MaxOutputJournalBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return outputJournalDocument{}, errors.Join(readErr, closeErr)
	}
	return decodeOutputJournal(encoded)
}

type outputJournalClassification uint8

const (
	outputJournalForeign outputJournalClassification = iota
	outputJournalStale
	outputJournalMatch
)

func classifyOutputJournal(document outputJournalDocument, session transfer.OutputSessionID, intent FilesystemOutputIntent, root outputRootBinding) outputJournalClassification {
	if document.Backend != string(filesystemOutputBackendID) ||
		document.OutputSession != encodeOutputBytes(session.Bytes()) ||
		document.RootLocator != root.locator || document.RootIdentity != root.identity {
		return outputJournalStale
	}
	if document.ShareInstance != encodeOutputBytes(intent.ShareInstance.Bytes()) ||
		document.ResumeIntent != encodeOutputBytes(intent.ResumeIntent.Bytes()) {
		return outputJournalForeign
	}
	return outputJournalMatch
}

func quarantineInactiveOutputJournal(root *os.Root, name string, session transfer.OutputSessionID) error {
	lockName := outputSessionLockName(session)
	lock, err := acquireOutputSessionLock(root, lockName)
	if err != nil {
		if errors.Is(err, ErrOutputSessionActive) {
			return errors.Join(ErrOutputDiscoveryUnsafe, err)
		}
		return err
	}
	quarantineErr := quarantineOutputJournal(root, name)
	return errors.Join(quarantineErr, lock.close(true))
}

func quarantineOutputJournal(root *os.Root, name string) error {
	for range outputNamespaceAllocationAttempts {
		var random [outputStageRandomBytes]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		target := outputQuarantinePrefix + encodeOutputFilenameToken(random[:]) + ".journal"
		if _, err := root.Lstat(target); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.Rename(name, target); err != nil {
			return fmt.Errorf("quarantine output journal %q: %w", name, err)
		}
		return nil
	}
	return errors.New("osfs: could not allocate a unique output quarantine name")
}

func abandonFilesystemOutputSession(session *FilesystemOutputSession) {
	if session == nil {
		return
	}
	session.mu.Lock()
	session.closed = true
	_ = session.lock.close(false)
	_ = session.root.Close()
	session.mu.Unlock()
}

func (s *FilesystemOutputSession) persistCheckpoint(binding transfer.OutputFileBinding, stage string, generation uint64, ranges content.RangeSet, published bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.NewOutputSessionError(ErrOutputSessionClosed, true)
	}
	current, found := s.journal.file(binding.Locator().CanonicalPath())
	if !found || !current.matches(binding) || current.Stage != stage {
		return transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
	}
	currentRanges, err := current.ranges()
	if err != nil || current.Published || current.Generation == math.MaxUint64 || generation != current.Generation+1 ||
		!journalRangesContain(ranges, currentRanges) {
		return transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
	}
	if published {
		if !slices.Equal(ranges.Ranges(), currentRanges.Ranges()) || !transfer.RangesCoverFile(binding.ExactSize(), ranges) {
			return transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
		}
	} else if slices.Equal(ranges.Ranges(), currentRanges.Ranges()) {
		return transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
	}
	candidate := s.journal.clone()
	candidate.put(journalFileFromBinding(binding, stage, generation, ranges, published))
	loaded, err := persistOutputJournal(s.root, s.journalName, candidate, s.hook)
	if err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	verifyPath := stage
	if published {
		verifyPath = filepath.FromSlash(binding.Locator().CanonicalPath())
	}
	if err := s.verifyOwnedPath(verifyPath, binding.ObjectIdentity(), binding.ExactSize()); err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	s.journal = loaded
	return nil
}

func journalRangesContain(available, required content.RangeSet) bool {
	availableRanges := available.Ranges()
	index := 0
	for _, current := range required.Ranges() {
		for index < len(availableRanges) && availableRanges[index].End < current.End {
			index++
		}
		if index == len(availableRanges) || availableRanges[index].Offset > current.Offset || availableRanges[index].End < current.End {
			return false
		}
	}
	return true
}
