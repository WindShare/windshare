package outputnamespace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

// ErrPositiveEntryEvidence marks failures that occurred after a fixed name was
// observed present. Recovery must not treat those failures as absence.
var ErrPositiveEntryEvidence = errors.New("osfs: output operation failed after positive entry evidence")

func ReadRecord(directory outputcap.Directory, name string, limit int) ([]byte, error) {
	result := ReadRecordWithCleanup(directory, name, limit)
	return result.Encoded, errors.Join(result.ReadError, result.CloseError)
}

// RecordReadResult preserves readable bytes when handle cleanup fails.
type RecordReadResult struct {
	Encoded    []byte
	ReadError  error
	CloseError error
}

// ReadRecordWithCleanup keeps namespace observation separate from handle
// cleanup. Install classifiers need the exact bytes even when Close fails so
// they can retain or adopt the generation that was actually reopened.
func ReadRecordWithCleanup(
	directory outputcap.Directory,
	name string,
	limit int,
) RecordReadResult {
	kind, err := ObserveExactEntry(directory, name)
	if err != nil {
		return RecordReadResult{ReadError: err}
	}
	if kind == outputcap.EntryAbsent {
		return RecordReadResult{ReadError: fs.ErrNotExist}
	}
	if kind != outputcap.EntryRegularFile {
		return RecordReadResult{ReadError: outputcap.ErrUnsafeNamespace}
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return RecordReadResult{ReadError: err, CloseError: closeFile(file)}
	}
	encoded, readErr := ReadFile(file, limit)
	return RecordReadResult{Encoded: encoded, ReadError: readErr, CloseError: file.Close()}
}

func ReadFile(file outputcap.File, limit int) ([]byte, error) {
	if file == nil || limit <= 0 {
		return nil, fmt.Errorf("%w: state record handle", outputcap.ErrUnsafeNamespace)
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 || size > uint64(limit) {
		return nil, fmt.Errorf("%w: state record size", outputcap.ErrUnsafeNamespace)
	}
	encoded := make([]byte, int(size))
	read, err := file.ReadAt(encoded, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if read != len(encoded) {
		return nil, io.ErrUnexpectedEOF
	}
	return encoded, nil
}

func VerifyRecord(directory outputcap.Directory, name string, expected []byte, limit int) error {
	actual, err := ReadRecord(directory, name, limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("%w: installed state record differs after reopen", outputcap.ErrUnsafeNamespace)
	}
	return nil
}

func validateEncodedState(encoded []byte, limit int) error {
	if len(encoded) == 0 || limit <= 0 || len(encoded) > limit {
		return fmt.Errorf("%w: encoded state exceeds its bound", outputcap.ErrUnsafeNamespace)
	}
	return nil
}

// DirectoryDisposition records why a directory capability was returned.
type DirectoryDisposition uint8

const (
	DirectoryAbsent DirectoryDisposition = iota
	DirectoryExisting
	DirectoryCreated
)

// DirectoryResult keeps namespace presence separate from the returned capability.
type DirectoryResult struct {
	Directory   outputcap.Directory
	Disposition DirectoryDisposition
}

func EnsureDirectory(parent outputcap.Directory, name string, private bool) (DirectoryResult, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return DirectoryResult{}, err
	}
	if kind != outputcap.EntryAbsent {
		if !exact || kind != outputcap.EntryDirectory {
			return DirectoryResult{}, outputcap.ErrUnsafeNamespace
		}
		directory, err := parent.OpenDirectory(name, private)
		if err != nil {
			return DirectoryResult{}, errors.Join(
				ErrPositiveEntryEvidence, err, closeDirectory(directory),
			)
		}
		return DirectoryResult{Directory: directory, Disposition: DirectoryExisting}, nil
	}
	directory, err := parent.CreateDirectory(name, private)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		collisionErr := err
		directory, err = parent.OpenDirectory(name, private)
		if err != nil {
			return DirectoryResult{}, errors.Join(
				ErrPositiveEntryEvidence, collisionErr, err, closeDirectory(directory),
			)
		}
		return DirectoryResult{Directory: directory, Disposition: DirectoryExisting}, nil
	}
	if err != nil {
		return DirectoryResult{}, errors.Join(err, closeDirectory(directory))
	}
	// A returned directory is immediately usable as authority. Persisting its
	// parent entry here prevents callers from accidentally relying on an
	// unsynchronised namespace cut.
	if err := errors.Join(directory.Sync(), parent.Sync()); err != nil {
		return DirectoryResult{}, errors.Join(err, directory.Close())
	}
	return DirectoryResult{Directory: directory, Disposition: DirectoryCreated}, nil
}

func OpenOptionalDirectory(
	directory outputcap.Directory,
	name string,
	private bool,
) (DirectoryResult, error) {
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return DirectoryResult{}, err
	}
	if kind == outputcap.EntryAbsent {
		return DirectoryResult{Disposition: DirectoryAbsent}, nil
	}
	if !exact || kind != outputcap.EntryDirectory {
		return DirectoryResult{}, outputcap.ErrUnsafeNamespace
	}
	opened, err := directory.OpenDirectory(name, private)
	if err != nil {
		return DirectoryResult{}, errors.Join(ErrPositiveEntryEvidence, err, closeDirectory(opened))
	}
	return DirectoryResult{Directory: opened, Disposition: DirectoryExisting}, nil
}

func ObserveExactEntry(directory outputcap.Directory, name string) (outputcap.EntryKind, error) {
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return outputcap.EntryAbsent, err
	}
	if kind != outputcap.EntryAbsent && !exact {
		return kind, outputcap.ErrUnsafeNamespace
	}
	return kind, nil
}

func closeFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}
