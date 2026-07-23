//go:build windows

package osfs

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"golang.org/x/sys/windows"
)

const (
	windowsRevisionIdentityBytes  = windowsPersistentFileIdentityBytes
	windowsRevisionCandidateBytes = windowsRevisionIdentityBytes + 24
	windowsFiletimeUnixOffset     = int64(116444736000000000)
)

func platformCatalogBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	return windowsCatalogObjectBaseline(file)
}

func newPlatformRootedRevisionSource(paths []string) (*RootedRevisionSource, error) {
	return NewWindowsRootedRevisionSource(paths)
}

type windowsMutationToken struct {
	identity   [windowsRevisionIdentityBytes]byte
	size       uint64
	lastWrite  int64
	changeTime int64
}

func (t windowsMutationToken) sourceIdentityBytes() []byte {
	result := make([]byte, windowsRevisionIdentityBytes)
	copy(result, t.identity[:])
	return result
}

func (t windowsMutationToken) candidateBytes() []byte {
	result := make([]byte, windowsRevisionCandidateBytes)
	copy(result, t.identity[:])
	binary.BigEndian.PutUint64(result[windowsRevisionIdentityBytes:windowsRevisionIdentityBytes+8], t.size)
	binary.BigEndian.PutUint64(result[windowsRevisionIdentityBytes+8:windowsRevisionIdentityBytes+16], uint64(t.lastWrite))
	binary.BigEndian.PutUint64(result[windowsRevisionIdentityBytes+16:windowsRevisionIdentityBytes+24], uint64(t.changeTime))
	return result
}

func (t windowsMutationToken) matches(record catalog.NodeRecord) bool {
	return t.size == record.Entry().ExpectedSize() &&
		subtle.ConstantTimeCompare(record.SourceIdentity().Bytes(), t.sourceIdentityBytes()) == 1 &&
		subtle.ConstantTimeCompare(record.VersionCandidate().Bytes(), t.candidateBytes()) == 1
}

func (t windowsMutationToken) sameOpenedRevision(other windowsMutationToken) bool {
	// ChangeTime closes the catalog-to-stable-open race, but a later rename also
	// changes it even though the write-excluding handle still names the exact
	// original object. Once FILE_SHARE_WRITE is denied, object identity, size,
	// and last-write time are the content invariants that remain meaningful.
	return t.identity == other.identity && t.size == other.size && t.lastWrite == other.lastWrite
}

func (t windowsMutationToken) modifiedTime() (catalog.ModifiedTime, error) {
	unixTicks := t.lastWrite - windowsFiletimeUnixOffset
	seconds := unixTicks / 10_000_000
	remainder := unixTicks % 10_000_000
	if remainder < 0 {
		seconds--
		remainder += 10_000_000
	}
	return catalog.NewModifiedTime(seconds, uint32(remainder*100), catalog.TimePrecisionNanoseconds)
}

type windowsRevisionFile interface {
	Token() (windowsMutationToken, error)
	ReadAt([]byte, int64) (int, error)
	Close() error
}

type windowsRevisionRoot interface {
	OpenStable(string) (windowsRevisionFile, error)
	Identity() ([windowsRevisionIdentityBytes]byte, error)
	Close() error
}

// windowsRevisionPlatform is the syscall boundary. Tests inject it so share
// modes, root selection, mutation cuts, and handle ownership are proven without
// weakening the production native-open path.
type windowsRevisionPlatform interface {
	OpenRoot(string) (windowsRevisionRoot, error)
	Token(*os.File) (windowsMutationToken, error)
}

type WindowsStabilityBinder struct {
	mu       sync.RWMutex
	platform windowsRevisionPlatform
	roots    []windowsRevisionRoot
	closed   bool
}

func NewWindowsStabilityBinder(rootPaths []string) (*WindowsStabilityBinder, error) {
	return newWindowsStabilityBinder(rootPaths, nativeWindowsRevisionPlatform{})
}

func newWindowsStabilityBinder(rootPaths []string, platform windowsRevisionPlatform) (*WindowsStabilityBinder, error) {
	if len(rootPaths) == 0 || len(rootPaths) > catalog.MaxRootSlots || platform == nil {
		return nil, content.ErrUnsupportedStability
	}
	roots := make([]windowsRevisionRoot, 0, len(rootPaths))
	for _, path := range rootPaths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, errors.Join(filesystemPathFailure("resolve Windows stable root", path, err), closeWindowsRevisionRoots(roots))
		}
		root, err := platform.OpenRoot(absolute)
		if err != nil {
			return nil, errors.Join(filesystemPathFailure("open Windows stable root", absolute, err), closeWindowsRevisionRoots(roots))
		}
		roots = append(roots, root)
	}
	return &WindowsStabilityBinder{platform: platform, roots: roots}, nil
}

// NewWindowsRootedRevisionSource is the R6 Windows factory. The native binder
// and its retained root handles are owned by the returned source and close with
// it; callers cannot accidentally substitute path-based reopen semantics.
func NewWindowsRootedRevisionSource(rootPaths []string) (*RootedRevisionSource, error) {
	binder, err := NewWindowsStabilityBinder(rootPaths)
	if err != nil {
		return nil, err
	}
	return newOwnedRootedRevisionSource(rootPaths, binder)
}

// WindowsCatalogBaseline captures the private catalog candidate from the
// already-open object. The later root-relative stable open must reproduce these
// exact values before a revision descriptor can be published.
func WindowsCatalogBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	if file == nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
	}
	if err := ensureSupportedWindowsVolume(windows.Handle(file.Fd())); err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	token, err := inspectWindowsMutationToken(windows.Handle(file.Fd()))
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, classifyWindowsIdentityError(err)
	}
	identity, err := catalog.NewSourceIdentity(token.sourceIdentityBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	candidate, err := catalog.NewVersionCandidate(token.candidateBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	return identity, candidate, nil
}

// windowsCatalogObjectBaseline extends the private catalog identity boundary to
// directories. Revision publication still uses WindowsCatalogBaseline and
// rejects directories; lazy catalog scans need the directory ChangeTime token
// so a generation cannot commit across an enumeration mutation.
func windowsCatalogObjectBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	if file == nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
	}
	handle := windows.Handle(file.Fd())
	if err := ensureSupportedWindowsVolume(handle); err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	token, err := inspectWindowsCatalogToken(handle)
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, classifyWindowsIdentityError(err)
	}
	identity, err := catalog.NewSourceIdentity(token.sourceIdentityBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	candidate, err := catalog.NewVersionCandidate(token.candidateBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	return identity, candidate, nil
}

func (b *WindowsStabilityBinder) BindStable(ctx context.Context, binding StableBinding) (content.StableFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.File == nil || binding.RelativePath == "" {
		return nil, content.ErrUnsupportedStability
	}
	relative := filepath.FromSlash(binding.RelativePath)
	if !filepath.IsLocal(relative) {
		return nil, content.ErrRevisionStale
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, content.ErrRevisionStoreClosed
	}
	slot := int(binding.RootSlot)
	if slot < 0 || slot >= len(b.roots) {
		return nil, content.ErrRevisionStale
	}
	before, err := b.platform.Token(binding.File)
	if err != nil {
		return nil, fmt.Errorf("inspect Windows revision before stable open: %w", err)
	}
	if !before.matches(binding.Record) {
		return nil, content.ErrRevisionStale
	}
	handle, err := b.roots[slot].OpenStable(relative)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			_ = handle.Close()
		}
	}()
	after, err := b.platform.Token(binding.File)
	if err != nil {
		return nil, fmt.Errorf("inspect Windows revision after stable open: %w", err)
	}
	stableToken, err := handle.Token()
	if err != nil {
		return nil, fmt.Errorf("inspect write-excluding Windows revision: %w", err)
	}
	if before != after || after != stableToken || !stableToken.matches(binding.Record) {
		return nil, content.ErrRevisionStale
	}
	modified, err := stableToken.modifiedTime()
	if err != nil {
		return nil, fmt.Errorf("represent Windows revision modified time: %w", err)
	}
	if err := binding.File.Close(); err != nil {
		return nil, fmt.Errorf("close preliminary Windows revision handle: %w", err)
	}
	owned = true
	return &windowsStableFile{handle: handle, baseline: stableToken, modified: modified}, nil
}

func (b *WindowsStabilityBinder) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	roots := b.roots
	b.roots = nil
	b.mu.Unlock()
	return closeWindowsRevisionRoots(roots)
}

func (b *WindowsStabilityBinder) ValidateRoots(roots []*os.Root) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return content.ErrRevisionStoreClosed
	}
	if len(roots) != len(b.roots) {
		return content.ErrUnsupportedStability
	}
	for index, root := range roots {
		file, err := root.Open(".")
		if err != nil {
			return fmt.Errorf("open Windows root authority %d: %w", index, err)
		}
		osIdentity, identityErr := inspectWindowsPersistentFileIdentity(windows.Handle(file.Fd()))
		closeErr := file.Close()
		if identityErr != nil || closeErr != nil {
			return fmt.Errorf("inspect Windows root authority %d: %w", index, errors.Join(identityErr, closeErr))
		}
		nativeIdentity, err := b.roots[index].Identity()
		if err != nil {
			return fmt.Errorf("inspect native Windows root authority %d: %w", index, err)
		}
		if nativeIdentity != osIdentity {
			return fmt.Errorf("%w: Windows root authority changed while it was retained", content.ErrUnsupportedStability)
		}
	}
	return nil
}

func closeWindowsRevisionRoots(roots []windowsRevisionRoot) error {
	var result error
	for index := len(roots) - 1; index >= 0; index-- {
		result = errors.Join(result, roots[index].Close())
	}
	return result
}

type windowsStableFile struct {
	mu       sync.RWMutex
	handle   windowsRevisionFile
	baseline windowsMutationToken
	modified catalog.ModifiedTime
	closed   bool
}

func (f *windowsStableFile) ExactSize() uint64                  { return f.baseline.size }
func (f *windowsStableFile) ModifiedTime() catalog.ModifiedTime { return f.modified }

func (f *windowsStableFile) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return content.ErrSourceDrift
	}
	return f.verifyLocked()
}

func (f *windowsStableFile) verifyLocked() error {
	current, err := f.handle.Token()
	if err != nil {
		return fmt.Errorf("inspect Windows stable source: %w", err)
	}
	if !current.sameOpenedRevision(f.baseline) {
		return content.ErrSourceDrift
	}
	return nil
}

func (f *windowsStableFile) ReadAt(ctx context.Context, destination []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset > math.MaxInt64 || uint64(len(destination)) > math.MaxInt64-offset {
		return 0, content.ErrBlockOutOfRange
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return 0, content.ErrSourceDrift
	}
	if err := f.verifyLocked(); err != nil {
		return 0, err
	}
	count, readErr := f.handle.ReadAt(destination, int64(offset))
	if err := f.verifyLocked(); err != nil {
		return count, err
	}
	if errors.Is(readErr, io.EOF) && count == len(destination) {
		return count, nil
	}
	return count, readErr
}

func (f *windowsStableFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.handle.Close()
}

type nativeWindowsRevisionPlatform struct{}

func (nativeWindowsRevisionPlatform) OpenRoot(path string) (windowsRevisionRoot, error) {
	handle, err := openWindowsRootHandle(path)
	if err != nil {
		return nil, classifyWindowsRootOpenError(err)
	}
	if err := ensureSupportedWindowsVolume(handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return &nativeWindowsRevisionRoot{handle: handle}, nil
}

func (nativeWindowsRevisionPlatform) Token(file *os.File) (windowsMutationToken, error) {
	token, err := inspectWindowsMutationToken(windows.Handle(file.Fd()))
	return token, classifyWindowsIdentityError(err)
}

type nativeWindowsRevisionRoot struct {
	mu     sync.Mutex
	handle windows.Handle
}

func (r *nativeWindowsRevisionRoot) Identity() ([windowsRevisionIdentityBytes]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return [windowsRevisionIdentityBytes]byte{}, content.ErrRevisionStoreClosed
	}
	identity, err := inspectWindowsPersistentFileIdentity(r.handle)
	return identity, classifyWindowsIdentityError(err)
}

func (r *nativeWindowsRevisionRoot) OpenStable(relative string) (windowsRevisionFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil, content.ErrRevisionStoreClosed
	}
	handle, err := openWindowsRelativeStableHandle(r.handle, relative)
	if err != nil {
		return nil, classifyWindowsStableOpenError(err)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap Windows stable revision handle")
	}
	return &nativeWindowsRevisionFile{file: file}, nil
}

func (r *nativeWindowsRevisionRoot) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil
	}
	handle := r.handle
	r.handle = windows.InvalidHandle
	return windows.CloseHandle(handle)
}

type nativeWindowsRevisionFile struct{ file *os.File }

func (f *nativeWindowsRevisionFile) Token() (windowsMutationToken, error) {
	token, err := inspectWindowsMutationToken(windows.Handle(f.file.Fd()))
	return token, classifyWindowsIdentityError(err)
}
func (f *nativeWindowsRevisionFile) ReadAt(destination []byte, offset int64) (int, error) {
	return f.file.ReadAt(destination, offset)
}
func (f *nativeWindowsRevisionFile) Close() error { return f.file.Close() }

type windowsFileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

func inspectWindowsMutationToken(handle windows.Handle) (windowsMutationToken, error) {
	identity, err := inspectWindowsPersistentFileIdentity(handle)
	if err != nil {
		return windowsMutationToken{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsMutationToken{}, err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return windowsMutationToken{}, content.ErrRevisionStale
	}
	var basic windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return windowsMutationToken{}, err
	}
	size := uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
	if size > catalog.MaxFileSize {
		return windowsMutationToken{}, content.ErrRevisionStale
	}
	return windowsMutationToken{
		identity: identity,
		size:     size, lastWrite: basic.LastWriteTime, changeTime: basic.ChangeTime,
	}, nil
}

func inspectWindowsCatalogToken(handle windows.Handle) (windowsMutationToken, error) {
	identity, err := inspectWindowsPersistentFileIdentity(handle)
	if err != nil {
		return windowsMutationToken{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsMutationToken{}, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return windowsMutationToken{}, content.ErrRevisionStale
	}
	var basic windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return windowsMutationToken{}, err
	}
	var size uint64
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		size = uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
		if size > catalog.MaxFileSize {
			return windowsMutationToken{}, content.ErrRevisionStale
		}
	}
	return windowsMutationToken{
		identity: identity, size: size, lastWrite: basic.LastWriteTime, changeTime: basic.ChangeTime,
	}, nil
}

func openWindowsRootHandle(path string) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(windowsNTPath(path))
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	return handle, normalizeWindowsNTError(err)
}

func openWindowsRelativeStableHandle(root windows.Handle, relative string) (windows.Handle, error) {
	if !filepath.IsLocal(relative) || filepath.IsAbs(relative) {
		return windows.InvalidHandle, content.ErrRevisionStale
	}
	name, err := windows.NewNTUnicodeString(relative)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root, ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, windowsStableDesiredAccess(), attributes, &status, nil, 0,
		// Denying FILE_SHARE_WRITE is the Windows stability proof. Sharing
		// delete preserves ordinary rename semantics while volume/file ID keeps
		// the opened object authoritative after a path replacement.
		windowsStableShareMode(),
		windows.FILE_OPEN,
		windowsStableOpenOptions(),
		0, 0,
	)
	return handle, normalizeWindowsNTError(err)
}

func windowsStableShareMode() uint32 {
	return windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE
}

func windowsStableDesiredAccess() uint32 { return windows.FILE_GENERIC_READ }

func windowsStableOpenOptions() uint32 {
	return windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT |
		windows.FILE_RANDOM_ACCESS | windows.FILE_SYNCHRONOUS_IO_NONALERT
}

func windowsNTPath(path string) string {
	clean := filepath.Clean(path)
	switch {
	case strings.HasPrefix(clean, `\\?\UNC\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\?\UNC\`)
	case strings.HasPrefix(clean, `\\?\`):
		return `\??\` + strings.TrimPrefix(clean, `\\?\`)
	case strings.HasPrefix(clean, `\\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\`)
	default:
		return `\??\` + clean
	}
}

func normalizeWindowsNTError(err error) error {
	if err == nil {
		return nil
	}
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		return status.Errno()
	}
	return err
}

func classifyWindowsStableOpenError(err error) error {
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return errors.Join(content.ErrUnsupportedStability, err)
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER), errors.Is(err, windows.ERROR_NOT_SUPPORTED),
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED):
		return errors.Join(content.ErrUnsupportedStability, err)
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND),
		errors.Is(err, windows.ERROR_CANT_ACCESS_FILE), errors.Is(err, windows.ERROR_REPARSE),
		errors.Is(err, windows.ERROR_REPARSE_OBJECT), errors.Is(err, windows.ERROR_REPARSE_POINT_ENCOUNTERED):
		return errors.Join(content.ErrRevisionStale, err)
	default:
		return err
	}
}

func classifyWindowsIdentityError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return err
}

func classifyWindowsRootOpenError(err error) error {
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return err
}

func ensureSupportedWindowsVolume(handle windows.Handle) error {
	volume, err := inspectWindowsVolume(handle)
	if err == nil {
		err = validateWindowsLocalPersistentVolume(volume)
	}
	if err != nil {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return nil
}
