package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type SpillRequest struct {
	ShareInstance ShareInstance
	AttemptID     ScanAttemptID
}

// SpillFactory is injected because temporary storage is an admission-controlled
// resource, not an implementation detail that scanners may create ad hoc.
type SpillFactory interface {
	NewWorkspace(context.Context, SpillRequest) (SpillWorkspace, error)
}

type SpillLifecycle interface {
	Recover(context.Context, ShareInstance) error
	Destroy(ShareInstance) error
}

type SpillWorkspace interface {
	Create(context.Context) (SpillWriter, error)
	Close() error
}

type SpillWriter interface {
	io.Writer
	Commit() (SpillObject, error)
	Abort() error
}

type SpillObject interface {
	Open(context.Context) (io.ReadCloser, error)
	Size() uint64
	Remove() error
}

type FileSpillFactory struct {
	root      string
	ownedRoot bool
}

func NewFileSpillFactory(root string) *FileSpillFactory {
	return &FileSpillFactory{root: root}
}

func defaultCatalogSpillFactory(backend CatalogBackend) (*FileSpillFactory, error) {
	if durable, ok := backend.(*FileCatalogBackend); ok {
		return NewFileSpillFactory(filepath.Join(durable.root, "sort")), nil
	}
	root, err := os.MkdirTemp("", "windshare-catalog-store-")
	if err != nil {
		return nil, fmt.Errorf("create private catalog spill root: %w", err)
	}
	return &FileSpillFactory{root: root, ownedRoot: true}, nil
}

func (f *FileSpillFactory) shareRoot(share ShareInstance) string {
	root := f.root
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, fmt.Sprintf("windshare-catalog-%x", share))
}

func (f *FileSpillFactory) Recover(ctx context.Context, share ShareInstance) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if share.IsZero() {
		return errors.New("catalog spill recovery requires a share identity")
	}
	root := f.shareRoot(share)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clean abandoned catalog sort runs: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create catalog share spill root: %w", err)
	}
	return nil
}

func (f *FileSpillFactory) Destroy(share ShareInstance) error {
	if share.IsZero() {
		return errors.New("catalog spill cleanup requires a share identity")
	}
	target := f.shareRoot(share)
	if f.ownedRoot {
		target = f.root
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove catalog share spill root: %w", err)
	}
	return nil
}

func (f *FileSpillFactory) NewWorkspace(ctx context.Context, request SpillRequest) (SpillWorkspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.ShareInstance.IsZero() || request.AttemptID.IsZero() {
		return nil, errors.New("catalog spill workspace requires share and attempt identities")
	}
	root := f.shareRoot(request.ShareInstance)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create catalog spill root: %w", err)
	}
	path, err := os.MkdirTemp(root, "windshare-catalog-sort-")
	if err != nil {
		return nil, fmt.Errorf("create catalog spill workspace: %w", err)
	}
	return &fileSpillWorkspace{path: path, objects: make(map[*fileSpillObject]struct{})}, nil
}

type fileSpillWorkspace struct {
	mu      sync.Mutex
	path    string
	closed  bool
	objects map[*fileSpillObject]struct{}
}

func (w *fileSpillWorkspace) Create(ctx context.Context) (SpillWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, errors.New("catalog spill workspace is closed")
	}
	file, err := os.CreateTemp(w.path, "run-")
	if err != nil {
		return nil, fmt.Errorf("create catalog spill run: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, fmt.Errorf("secure catalog spill run: %w", err)
	}
	return &fileSpillWriter{workspace: w, file: file, path: file.Name()}, nil
}

func (w *fileSpillWorkspace) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	path := w.path
	w.objects = nil
	w.mu.Unlock()
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove catalog spill workspace: %w", err)
	}
	return nil
}

type fileSpillWriter struct {
	mu        sync.Mutex
	workspace *fileSpillWorkspace
	file      *os.File
	path      string
	size      uint64
	finished  bool
}

func (w *fileSpillWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return 0, errors.New("catalog spill writer is finished")
	}
	n, err := w.file.Write(data)
	w.size += uint64(n)
	return n, err
}

func (w *fileSpillWriter) Commit() (SpillObject, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil, errors.New("catalog spill writer is finished")
	}
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		_ = os.Remove(w.path)
		w.finished = true
		return nil, fmt.Errorf("sync catalog spill run: %w", err)
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(w.path)
		w.finished = true
		return nil, fmt.Errorf("close catalog spill run: %w", err)
	}
	w.finished = true
	object := &fileSpillObject{workspace: w.workspace, path: w.path, size: w.size}
	w.workspace.mu.Lock()
	if w.workspace.closed {
		w.workspace.mu.Unlock()
		_ = os.Remove(w.path)
		return nil, errors.New("catalog spill workspace closed during run commit")
	}
	w.workspace.objects[object] = struct{}{}
	w.workspace.mu.Unlock()
	return object, nil
}

func (w *fileSpillWriter) Abort() error {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return nil
	}
	w.finished = true
	file := w.file
	path := w.path
	w.mu.Unlock()
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

type fileSpillObject struct {
	mu        sync.Mutex
	workspace *fileSpillWorkspace
	path      string
	size      uint64
	removed   bool
}

func (o *fileSpillObject) Open(ctx context.Context) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.removed {
		return nil, errors.New("catalog spill object was removed")
	}
	file, err := os.Open(filepath.Clean(o.path))
	if err != nil {
		return nil, fmt.Errorf("open catalog spill object: %w", err)
	}
	return file, nil
}

func (o *fileSpillObject) Size() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.size
}

func (o *fileSpillObject) Remove() error {
	o.mu.Lock()
	if o.removed {
		o.mu.Unlock()
		return nil
	}
	o.removed = true
	path := o.path
	o.mu.Unlock()
	err := os.Remove(path)
	o.workspace.mu.Lock()
	if o.workspace.objects != nil {
		delete(o.workspace.objects, o)
	}
	o.workspace.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
