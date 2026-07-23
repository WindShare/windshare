package osfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

const (
	filesystemOutputBackendName       = "windshare/cli-osfs/v2"
	outputJournalPrefix               = ".wsresume-output-"
	outputStagePrefix                 = ".wsresume-output-stage-"
	outputStageRandomBytes            = 16
	outputNamespaceAllocationAttempts = 16
	maximumPathDiagnosticBytes        = 256
	dirPerm                           = 0o755
	filePerm                          = 0o644
)

var (
	ErrDrift            = errors.New("osfs: source changed; create a new share")
	ErrNotInSnapshot    = errors.New("osfs: path is not in the snapshot")
	ErrOutOfRange       = errors.New("osfs: byte range is out of bounds")
	ErrPathEscape       = errors.New("osfs: path escapes the output root")
	ErrPathTooLong      = errors.New("osfs: output path exceeds the platform limit")
	ErrAlreadyExists    = errors.New("osfs: output file already exists")
	ErrOwnedFileMissing = errors.New("osfs: an output owned by this transaction is missing")
	ErrOwnershipRecord  = errors.New("osfs: could not persist output ownership")
)

var filesystemOutputBackendID = func() transfer.OutputBackendID {
	backend, err := transfer.NewOutputBackendID(filesystemOutputBackendName)
	if err != nil {
		panic(err)
	}
	return backend
}()

func encodeOutputBytes(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func decodeOutputBytes(encoded string, size int) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != size {
		return nil, ErrOutputJournalCorrupt
	}
	return raw, nil
}

// Lowercase hexadecimal keeps identity-derived authority names injective on
// case-insensitive filesystems. Base64url is not safe here because Windows can
// collapse distinct session identities whose encodings differ only by case.
func encodeOutputFilenameToken(raw []byte) string { return hex.EncodeToString(raw) }

func outputIntentLockName(share catalog.ShareInstance, intent OutputResumeIntent) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("windshare/output-intent-lock/v1\x00"))
	_, _ = hash.Write([]byte(filesystemOutputBackendID))
	_, _ = hash.Write(share.Bytes())
	_, _ = hash.Write(intent[:])
	return outputIntentLockPrefix + encodeOutputFilenameToken(hash.Sum(nil)) + ".lock"
}

func outputSessionIDFromJournalName(name string) (transfer.OutputSessionID, bool) {
	if !strings.HasPrefix(name, outputJournalPrefix) || !strings.HasSuffix(name, ".journal") {
		return transfer.OutputSessionID{}, false
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(name, outputJournalPrefix), ".journal")
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != transfer.OutputSessionIdentityBytes || encodeOutputFilenameToken(raw) != encoded {
		return transfer.OutputSessionID{}, false
	}
	session, err := transfer.OutputSessionIDFromBytes(raw)
	return session, err == nil && !session.IsZero()
}

func validOutputStageName(name string) bool {
	if filepath.Base(name) != name || !strings.HasPrefix(name, outputStagePrefix) {
		return false
	}
	encoded := strings.TrimPrefix(name, outputStagePrefix)
	raw, err := hex.DecodeString(encoded)
	return err == nil && len(raw) == outputStageRandomBytes && encodeOutputFilenameToken(raw) == encoded
}

func newOutputStageName() (string, error) {
	var random [outputStageRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return outputStagePrefix + encodeOutputFilenameToken(random[:]), nil
}

func createOutputJournalTemp(root *os.Root) (*os.File, string, error) {
	for range outputNamespaceAllocationAttempts {
		var random [outputStageRandomBytes]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := outputJournalTempPrefix + encodeOutputFilenameToken(random[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("osfs: could not allocate a unique output journal temporary name")
}

func outputSessionLockName(session transfer.OutputSessionID) string {
	return outputJournalPrefix + encodeOutputFilenameToken(session.Bytes()) + ".lock"
}

type outputSessionLock struct {
	root *os.Root
	file *os.File
	name string
}

func acquireOutputSessionLock(root *os.Root, name string) (*outputSessionLock, error) {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, err
	}
	if err := lockOutputFile(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &outputSessionLock{root: root, file: file, name: name}, nil
}

func (l *outputSessionLock) close(remove bool) error {
	if l == nil || l.file == nil {
		return nil
	}
	var removeErr error
	if remove {
		removeErr = l.root.Remove(l.name)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
	}
	unlockErr := unlockOutputFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(removeErr, unlockErr, closeErr)
}

func (s *FilesystemOutputSession) removeOwnedPath(path string, identity transfer.OutputObjectIdentity, size uint64) error {
	file, err := openOutputRemovalFile(s.root, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := s.verifyFile(file, identity, size); err != nil {
		closeErr := file.Close()
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrOwnedFileMissing) {
			return closeErr
		}
		return errors.Join(err, closeErr)
	}
	removeErr := removeOpenedOutputFile(s.root, path, file)
	closeErr := file.Close()
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(removeErr, closeErr)
}

// pathDiagnosticError keeps native path details out of the machine-readable
// cause chain while preserving a bounded, escaped location for operators.
type pathDiagnosticError struct {
	operation string
	path      string
	category  error
	cause     error
}

func (failure *pathDiagnosticError) Error() string {
	detail := "operation failed"
	if failure.category != nil {
		detail = strings.TrimPrefix(failure.category.Error(), "osfs: ")
	}
	return fmt.Sprintf("%s %s: %s", failure.operation, quotePathForDiagnostic(failure.path), detail)
}

func (failure *pathDiagnosticError) Unwrap() error { return failure.cause }

func (failure *pathDiagnosticError) Is(target error) bool {
	return failure.category != nil && errors.Is(failure.category, target)
}

func categorizedPathFailure(operation, path string, category, cause error) error {
	return &pathDiagnosticError{operation: operation, path: path, category: category, cause: cause}
}

func filesystemPathFailure(operation, path string, cause error) error {
	var category error
	if isPathTooLongError(cause) {
		category = ErrPathTooLong
	}
	return categorizedPathFailure(operation, path, category, cause)
}

func quotePathForDiagnostic(path string) string {
	end := min(len(path), maximumPathDiagnosticBytes)
	for end < len(path) && end > 0 && !utf8.RuneStart(path[end]) {
		end--
	}
	truncated := end < len(path)
	for {
		candidate := path[:end]
		if truncated {
			candidate += "…"
		}
		quoted := strconv.Quote(candidate)
		if len(quoted) <= maximumPathDiagnosticBytes {
			return quoted
		}
		truncated = true
		if end == 0 {
			return strconv.Quote("…")
		}
		end--
		for end > 0 && !utf8.RuneStart(path[end]) {
			end--
		}
	}
}
