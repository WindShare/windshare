package osfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/core/manifest"
)

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// OwnershipLedger grants reopen authority for exact canonical file paths.
// RecordCreated must durably record the path before returning when the ledger
// is persistent and make subsequent Owns calls return true; the receive
// transaction may only mark a block complete after WriteRange succeeds.
type OwnershipLedger interface {
	Owns(path string) bool
	RecordCreated(path string) error
}

// SinkOptions carries capabilities, not modes. A nil ledger gives a fresh
// in-memory transaction; resumable callers inject their journal-backed ledger.
type SinkOptions struct {
	Ownership OwnershipLedger
}

type memoryOwnership struct {
	paths map[string]struct{}
}

func newMemoryOwnership() *memoryOwnership {
	return &memoryOwnership{paths: make(map[string]struct{})}
}

func (m *memoryOwnership) Owns(path string) bool {
	_, ok := m.paths[path]
	return ok
}

func (m *memoryOwnership) RecordCreated(path string) error {
	m.paths[path] = struct{}{}
	return nil
}

// randomAccessFile and rootedFilesystem are defined where Sink consumes them.
// The narrow boundary makes path validation/ownership behavior testable without
// weakening production containment, which is provided by os.Root.
type randomAccessFile interface {
	WriteAt(data []byte, off int64) (int, error)
	Close() error
}

type rootedFilesystem interface {
	MkdirAll(name string, perm fs.FileMode) error
	OpenFile(name string, flag int, perm fs.FileMode) (randomAccessFile, error)
	Chtimes(name string, atime, mtime time.Time) error
	Close() error
}

type osRootFilesystem struct {
	root *os.Root
}

func (r *osRootFilesystem) MkdirAll(name string, perm fs.FileMode) error {
	return r.root.MkdirAll(name, perm)
}

func (r *osRootFilesystem) OpenFile(name string, flag int, perm fs.FileMode) (randomAccessFile, error) {
	return r.root.OpenFile(name, flag, perm)
}

func (r *osRootFilesystem) Chtimes(name string, atime, mtime time.Time) error {
	return r.root.Chtimes(name, atime, mtime)
}

func (r *osRootFilesystem) Close() error { return r.root.Close() }

// Sink retains an opened root capability for its lifetime. Lexical validation
// remains defense in depth, while every filesystem operation is independently
// constrained by the root handle even if a local symlink or junction appears.
type Sink struct {
	rootPath  string
	root      rootedFilesystem
	ownership OwnershipLedger

	// The ledger need not provide its own synchronization: Sink serializes the
	// ownership decision, exclusive create, and durable record as one operation.
	mu sync.Mutex
}

// NewSink creates the output root and anchors all later operations to a handle.
func NewSink(rootPath string, options SinkOptions) (*Sink, error) {
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, filesystemPathFailure("osfs: resolve output root", rootPath, err)
	}
	if exceedsPathLimit(abs) {
		return nil, categorizedPathFailure("osfs: resolve output root", abs, ErrPathTooLong, nil)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, filesystemPathFailure("osfs: create output root", abs, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, filesystemPathFailure("osfs: open output root", abs, err)
	}
	return newSinkWithFilesystem(abs, &osRootFilesystem{root: root}, options), nil
}

func newSinkWithFilesystem(rootPath string, root rootedFilesystem, options SinkOptions) *Sink {
	ownership := options.Ownership
	if ownership == nil {
		ownership = newMemoryOwnership()
	}
	return &Sink{rootPath: rootPath, root: root, ownership: ownership}
}

// Close releases the root handle. Callers should close a Sink after the
// receive transaction finishes; already-opened output files are closed per write.
func (s *Sink) Close() error {
	if err := s.root.Close(); err != nil {
		return pathFailure("osfs: close output root", s.rootPath, err)
	}
	return nil
}

type resolvedPath struct {
	relative string
	absolute string
}

// resolve performs all pure checks before a root operation. The os.Root call is
// still authoritative containment; this independent lexical layer gives clear
// product errors for hostile manifests instead of platform-specific failures.
func (s *Sink) resolve(path string) (resolvedPath, error) {
	if err := manifest.ValidatePath(path); err != nil {
		return resolvedPath{}, err
	}
	relative := filepath.FromSlash(path)
	if !filepath.IsLocal(relative) {
		return resolvedPath{}, fmt.Errorf("%w: %s", ErrPathEscape, manifest.QuotePathForDiagnostic(path))
	}
	absolute := filepath.Join(s.rootPath, relative)
	rel, err := filepath.Rel(s.rootPath, absolute)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return resolvedPath{}, fmt.Errorf("%w: %s", ErrPathEscape, manifest.QuotePathForDiagnostic(path))
	}
	if exceedsPathLimit(absolute) {
		return resolvedPath{}, fmt.Errorf("%w: %s", ErrPathTooLong, manifest.QuotePathForDiagnostic(absolute))
	}
	return resolvedPath{relative: relative, absolute: absolute}, nil
}

func (s *Sink) EnsureDir(path string) error {
	resolved, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := s.root.MkdirAll(resolved.relative, dirPerm); err != nil {
		return filesystemPathFailure("osfs: create directory", resolved.absolute, err)
	}
	return nil
}

// WriteRange uses positioned writes because blocks may arrive out of order.
func (s *Sink) WriteRange(path string, off int64, data []byte) error {
	resolved, err := s.resolve(path)
	if err != nil {
		return err
	}
	if off < 0 || off > math.MaxInt64-int64(len(data)) {
		return fmt.Errorf("%w: %s off=%d length=%d", ErrOutOfRange, manifest.QuotePathForDiagnostic(path), off, len(data))
	}
	parent := filepath.Dir(resolved.relative)
	if parent != "." {
		if err := s.root.MkdirAll(parent, dirPerm); err != nil {
			return filesystemPathFailure("osfs: create parent for", resolved.absolute, err)
		}
	}
	file, err := s.openOutput(path, resolved)
	if err != nil {
		return err
	}
	written, writeErr := file.WriteAt(data, off)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if writeErr != nil {
		writeErr = pathFailure("osfs: write", resolved.absolute, writeErr)
		if closeErr != nil {
			return errors.Join(writeErr, pathFailure("osfs: close output", resolved.absolute, closeErr))
		}
		return writeErr
	}
	if closeErr != nil {
		return pathFailure("osfs: close output", resolved.absolute, closeErr)
	}
	return nil
}

func (s *Sink) openOutput(path string, resolved resolvedPath) (randomAccessFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	owned := s.ownership.Owns(path)
	flags := os.O_WRONLY
	if !owned {
		// O_EXCL makes the ownership boundary atomic. A preflight stat would
		// leave a window in which an unrelated file could replace the target.
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := s.root.OpenFile(resolved.relative, flags, filePerm)
	if !owned && errors.Is(err, fs.ErrExist) {
		return nil, categorizedPathFailure("osfs: open output", resolved.absolute, ErrAlreadyExists, err)
	}
	if owned && errors.Is(err, fs.ErrNotExist) {
		return nil, categorizedPathFailure("osfs: open output", resolved.absolute, ErrOwnedFileMissing, err)
	}
	if err != nil {
		return nil, filesystemPathFailure("osfs: open output", resolved.absolute, err)
	}
	if !owned {
		if err := s.ownership.RecordCreated(path); err != nil {
			recordErr := categorizedPathFailure("osfs: record output ownership for", path, ErrOwnershipRecord, err)
			if closeErr := file.Close(); closeErr != nil {
				return nil, errors.Join(recordErr, pathFailure("osfs: close unowned output", resolved.absolute, closeErr))
			}
			return nil, recordErr
		}
	}
	return file, nil
}

// SetMTime is root-relative as well; directory mtimes are applied by the caller
// after descendants so later child writes do not invalidate them.
func (s *Sink) SetMTime(path string, mtime int64) error {
	resolved, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := s.root.Chtimes(resolved.relative, time.Time{}, time.UnixMilli(mtime)); err != nil {
		return filesystemPathFailure("osfs: set mtime for", resolved.absolute, err)
	}
	return nil
}
