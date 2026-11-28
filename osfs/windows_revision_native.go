//go:build windows

package osfs

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/osfs/internal/linkresolve"
	"golang.org/x/sys/windows"
)

const (
	windowsRevisionIdentityBytes  = 25
	windowsRevisionCandidateBytes = windowsRevisionIdentityBytes + 24
	windowsFiletimeUnixOffset     = int64(116444736000000000)
	windowsIdentityFullWidth      = byte(1)
	windowsIdentityLegacy         = byte(2)
)

type windowsRevisionFileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func inspectWindowsHandleIdentity(handle windows.Handle) ([windowsRevisionIdentityBytes]byte, error) {
	return inspectWindowsFileIdentityWith(handle, nativeWindowsRevisionMetadata{})
}

func inspectWindowsFileIdentityWith(handle windows.Handle, api windowsRevisionMetadata) ([windowsRevisionIdentityBytes]byte, error) {
	information, err := api.FileInformation(handle)
	if err != nil {
		return [windowsRevisionIdentityBytes]byte{}, err
	}
	return inspectWindowsFileIdentity(handle, information, api)
}

// Legacy file IDs are useful for comparing simultaneously open handles, but
// cannot prove content continuity after close: FAT can reuse or rename them.
// Keeping the identity formats separate also prevents a truncated ReFS ID from
// silently comparing equal to a full-width ID.
func inspectWindowsFileIdentity(handle windows.Handle, information windows.ByHandleFileInformation, api windowsRevisionMetadata) ([windowsRevisionIdentityBytes]byte, error) {
	var identity [windowsRevisionIdentityBytes]byte
	full, err := api.FileIdentity(handle)
	if err == nil {
		if full.FileID == [16]byte{} {
			return identity, fmt.Errorf("%w: Windows provider returned an empty full-width file identity", content.ErrUnsupportedStability)
		}
		identity[0] = windowsIdentityFullWidth
		binary.BigEndian.PutUint64(identity[1:9], full.VolumeSerialNumber)
		copy(identity[9:], full.FileID[:])
		return identity, nil
	}
	if !isWindowsCapabilityUnavailable(err) {
		return identity, err
	}
	filesystem, volumeErr := api.Filesystem(handle)
	if volumeErr != nil {
		return identity, fmt.Errorf("%w: identify Windows legacy file-ID semantics: %w", content.ErrUnsupportedStability, volumeErr)
	}
	if filesystem == "" || strings.EqualFold(filesystem, "ReFS") {
		return identity, fmt.Errorf("%w: filesystem %q requires an unambiguous full-width file identity: %w", content.ErrUnsupportedStability, filesystem, err)
	}
	index := uint64(information.FileIndexHigh)<<32 | uint64(information.FileIndexLow)
	if index == 0 {
		// FAT uses file ID zero for the volume root. An arbitrary zero ID from
		// a network provider is partial information, not an object identity.
		path, pathErr := api.FinalPath(handle)
		volume := filepath.VolumeName(path)
		if pathErr != nil || volume == "" || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
			!strings.EqualFold(filepath.Clean(path), filepath.Clean(volume+`\`)) {
			return identity, fmt.Errorf("%w: Windows provider has no usable legacy file identity: %w", content.ErrUnsupportedStability, errors.Join(err, pathErr))
		}
	}
	identity[0] = windowsIdentityLegacy
	binary.BigEndian.PutUint64(identity[1:9], uint64(information.VolumeSerialNumber))
	binary.BigEndian.PutUint64(identity[9:17], index)
	binary.BigEndian.PutUint64(identity[17:25], uint64(information.CreationTime.HighDateTime)<<32|uint64(information.CreationTime.LowDateTime))
	return identity, nil
}

// Metadata capabilities are queried on the actual handle. Filesystem names
// guard ReFS legacy-ID collisions; they never grant a missing sharing mode.
type windowsRevisionMetadata interface {
	FileIdentity(windows.Handle) (windowsRevisionFileIDInfo, error)
	FileInformation(windows.Handle) (windows.ByHandleFileInformation, error)
	BasicInformation(windows.Handle) (windowsFileBasicInfo, error)
	AttributeTag(windows.Handle) (windowsAttributeTagInfo, error)
	FileType(windows.Handle) (uint32, error)
	// PlaceholderState decodes Cloud Files state for handle-sourced attributes
	// with FILE_ATTRIBUTE_EA (0x00040000) already cleared.
	PlaceholderState(attributes, reparseTag uint32) uint32
	Filesystem(windows.Handle) (string, error)
	FinalPath(windows.Handle) (string, error)
}

type nativeWindowsRevisionMetadata struct{}

func (nativeWindowsRevisionMetadata) FileIdentity(handle windows.Handle) (windowsRevisionFileIDInfo, error) {
	var information windowsRevisionFileIDInfo
	err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)))
	return information, err
}

func (nativeWindowsRevisionMetadata) FileInformation(handle windows.Handle) (windows.ByHandleFileInformation, error) {
	var information windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(handle, &information)
	return information, err
}

func (nativeWindowsRevisionMetadata) BasicInformation(handle windows.Handle) (windowsFileBasicInfo, error) {
	var basic windowsFileBasicInfo
	err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)))
	return basic, err
}

func (nativeWindowsRevisionMetadata) AttributeTag(handle windows.Handle) (windowsAttributeTagInfo, error) {
	return queryWindowsHandleAttributeTag(handle)
}

func (nativeWindowsRevisionMetadata) FileType(handle windows.Handle) (uint32, error) {
	return windows.GetFileType(handle)
}

func (nativeWindowsRevisionMetadata) PlaceholderState(attributes, reparseTag uint32) uint32 {
	return queryWindowsHandlePlaceholderState(attributes, reparseTag)
}

func (nativeWindowsRevisionMetadata) Filesystem(handle windows.Handle) (string, error) {
	var filesystem [32]uint16
	err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem)))
	return windows.UTF16ToString(filesystem[:]), err
}

func (nativeWindowsRevisionMetadata) FinalPath(handle windows.Handle) (string, error) {
	return linkresolve.WindowsFinalPathByHandle(handle)
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
	candidate := record.VersionCandidate().Bytes()
	return len(candidate) == windowsRevisionCandidateBytes && t.size == record.Entry().ExpectedSize() &&
		subtle.ConstantTimeCompare(record.SourceIdentity().Bytes(), t.sourceIdentityBytes()) == 1 &&
		subtle.ConstantTimeCompare(candidate, t.candidateBytes()) == 1
}

func (t windowsMutationToken) sameCatalogEvidence(other windowsMutationToken) bool {
	return t == other
}

func (t windowsMutationToken) sameOpenedRevision(other windowsMutationToken) bool {
	// ChangeTime helps compare the catalog candidate, but a later rename also
	// changes it even though the write-excluding handle still names the exact
	// original object. Once FILE_SHARE_WRITE is denied, object identity, size,
	// and last-write time are the content invariants that remain meaningful.
	sameIdentity := t.identity == other.identity
	if t.identity[0] == windowsIdentityLegacy && other.identity[0] == windowsIdentityLegacy {
		// Renaming a FAT file can change its directory-slot ID. The retained
		// write-excluding handle remains authoritative for this open lifetime.
		sameIdentity = true
	}
	return sameIdentity && t.size == other.size && t.lastWrite == other.lastWrite
}

func (t windowsMutationToken) modifiedTime() (catalog.ModifiedTime, error) {
	if t.lastWrite == 0 {
		return catalog.ModifiedTime{}, nil
	}
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

type nativeWindowsRevisionPlatform struct{ metadata windowsRevisionMetadata }

func (p nativeWindowsRevisionPlatform) metadataAPI() windowsRevisionMetadata {
	if p.metadata != nil {
		return p.metadata
	}
	return nativeWindowsRevisionMetadata{}
}

func (p nativeWindowsRevisionPlatform) OpenRoot(path string) (windowsRevisionRoot, error) {
	handle, err := openWindowsRootHandle(path)
	if err != nil {
		return nil, classifyWindowsRootOpenError(err)
	}
	return adoptNativeWindowsRevisionRoot(handle, p.metadataAPI())
}

func (p nativeWindowsRevisionPlatform) Token(file *os.File) (windowsMutationToken, error) {
	if file == nil {
		return windowsMutationToken{}, content.ErrUnsupportedStability
	}
	defer runtime.KeepAlive(file)
	token, err := inspectWindowsFileToken(windows.Handle(file.Fd()), p.metadataAPI())
	return token, classifyWindowsIdentityError(err)
}

type windowsRootRevisionMetadata struct {
	windowsRevisionMetadata
	filesystem    string
	filesystemErr error
}

func (metadata windowsRootRevisionMetadata) Filesystem(windows.Handle) (string, error) {
	return metadata.filesystem, metadata.filesystemErr
}

// A retained selection can move while sharing. Duplicating its handle keeps
// native write exclusion bound to that original directory instead of its name.
func retainWindowsRevisionRoot(authority *os.Root) (windowsRevisionRoot, error) {
	handle, err := duplicateRootHandleNoRecall(authority)
	if err != nil {
		return nil, err
	}
	return adoptNativeWindowsRevisionRoot(handle, nativeWindowsRevisionMetadata{})
}

func adoptNativeWindowsRevisionRoot(handle windows.Handle, api windowsRevisionMetadata) (*nativeWindowsRevisionRoot, error) {
	if _, err := inspectWindowsFileIdentityWith(handle, api); err != nil {
		return nil, errors.Join(classifyWindowsIdentityError(err), windows.CloseHandle(handle))
	}
	filesystem, filesystemErr := api.Filesystem(handle)
	metadata := windowsRootRevisionMetadata{windowsRevisionMetadata: api, filesystem: filesystem, filesystemErr: filesystemErr}
	return &nativeWindowsRevisionRoot{handle: handle, metadata: metadata}, nil
}

// r.handle is immutable until Close, and Windows root-relative NtCreateFile is
// safe for concurrent opens on the same directory handle. Using RWMutex avoids
// serializing sibling file opens behind each other while still blocking Close.
type nativeWindowsRevisionRoot struct {
	mu       sync.RWMutex
	handle   windows.Handle
	metadata windowsRevisionMetadata
}

func (r *nativeWindowsRevisionRoot) Identity() ([windowsRevisionIdentityBytes]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return [windowsRevisionIdentityBytes]byte{}, content.ErrRevisionStoreClosed
	}
	identity, err := inspectWindowsFileIdentityWith(r.handle, r.metadata)
	return identity, classifyWindowsIdentityError(err)
}

func (r *nativeWindowsRevisionRoot) OpenStable(relative string) (windowsRevisionFile, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	return &nativeWindowsRevisionFile{file: file, metadata: r.metadata}, nil
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

type nativeWindowsRevisionFile struct {
	file             *os.File
	metadata         windowsRevisionMetadata
	verifiedDiskFile atomic.Bool
}

func (f *nativeWindowsRevisionFile) Token() (windowsMutationToken, error) {
	if f == nil || f.file == nil {
		return windowsMutationToken{}, content.ErrUnsupportedStability
	}
	defer runtime.KeepAlive(f.file)
	knownDisk := f.verifiedDiskFile.Load()
	token, directory, err := inspectWindowsObjectMetadataWith(windows.Handle(f.file.Fd()), f.metadata, knownDisk)
	if err == nil && directory {
		err = content.ErrRevisionStale
	}
	if err == nil {
		f.verifiedDiskFile.Store(true)
	} else if isWindowsPlaceholderError(err) {
		err = content.ErrRevisionStale
	}
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
	return inspectWindowsFileToken(handle, nativeWindowsRevisionMetadata{})
}

func inspectWindowsFileToken(handle windows.Handle, api windowsRevisionMetadata) (windowsMutationToken, error) {
	token, directory, err := inspectWindowsObjectMetadata(handle, api)
	if err == nil && directory {
		err = content.ErrRevisionStale
	}
	if isWindowsPlaceholderError(err) {
		err = content.ErrRevisionStale
	}
	return token, err
}

func inspectWindowsCatalogToken(handle windows.Handle) (windowsMutationToken, error) {
	token, _, err := inspectWindowsObjectMetadata(handle, nativeWindowsRevisionMetadata{})
	return token, err
}

func inspectWindowsObjectMetadata(handle windows.Handle, api windowsRevisionMetadata) (windowsMutationToken, bool, error) {
	return inspectWindowsObjectMetadataWith(handle, api, false)
}

func inspectWindowsObjectMetadataWith(
	handle windows.Handle,
	api windowsRevisionMetadata,
	knownDiskFile bool,
) (windowsMutationToken, bool, error) {
	information, err := api.FileInformation(handle)
	if err != nil {
		return windowsMutationToken{}, false, err
	}
	kind, err := verifyWindowsHandleMetadata(handle, information.FileAttributes, api, knownDiskFile)
	if err != nil {
		return windowsMutationToken{}, false, err
	}
	directory := kind == sourceEntryDirectory
	identity, err := inspectWindowsFileIdentity(handle, information, api)
	if err != nil {
		return windowsMutationToken{}, directory, err
	}
	basic, err := api.BasicInformation(handle)
	if err != nil {
		if !isWindowsCapabilityUnavailable(err) {
			return windowsMutationToken{}, directory, err
		}
		// Basic metadata still describes the discovery candidate when extended
		// change time is absent; the deny-write handle proves reads while open.
		basic = windowsFileBasicInfo{LastWriteTime: int64(uint64(information.LastWriteTime.HighDateTime)<<32 | uint64(information.LastWriteTime.LowDateTime))}
	}
	var size uint64
	if !directory {
		size = uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
		if size > catalog.MaxFileSize {
			return windowsMutationToken{}, directory, content.ErrRevisionStale
		}
	}
	return windowsMutationToken{
		identity: identity, size: size, lastWrite: basic.LastWriteTime, changeTime: basic.ChangeTime,
	}, directory, nil
}

func verifyWindowsHandleMetadata(
	handle windows.Handle,
	attributes uint32,
	api windowsRevisionMetadata,
	knownDiskFile bool,
) (sourceEntryKind, error) {
	// Ordinary local files have no reparse point and reuse FileInformation with
	// zero extra AttributeTag or GetFileType queries.
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		kind, _ := classifyWindowsEntry(windowsEntryFacts{
			source:        windowsAttributesFromHandle,
			attributes:    attributes,
			hasAttributes: true,
		})
		switch kind {
		case sourceEntryDirectory, sourceEntryRegularFile:
			return kind, nil
		case sourceEntryCloudPlaceholder:
			return 0, errSourceCloudPlaceholder
		default:
			return 0, content.ErrRevisionStale
		}
	}
	tagInfo, err := api.AttributeTag(handle)
	if err != nil {
		return 0, err
	}
	hasReparseTag := tagInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	var state uint32
	if hasReparseTag && isCloudReparseTag(tagInfo.ReparseTag) {
		state = api.PlaceholderState(tagInfo.FileAttributes&^windowsAttrEA, tagInfo.ReparseTag)
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source:           windowsAttributesFromHandle,
		attributes:       tagInfo.FileAttributes,
		hasAttributes:    true,
		hasReparseTag:    hasReparseTag,
		reparseTag:       tagInfo.ReparseTag,
		placeholderState: state,
	})
	switch kind {
	case sourceEntryDirectory:
		return sourceEntryDirectory, nil
	case sourceEntryRegularFile:
		if audit.hydratedCloud && !knownDiskFile {
			fileType, ftErr := api.FileType(handle)
			if ftErr != nil {
				return 0, ftErr
			}
			if fileType != windows.FILE_TYPE_DISK {
				return 0, content.ErrRevisionStale
			}
		}
		return sourceEntryRegularFile, nil
	case sourceEntryCloudPlaceholder:
		return 0, errSourceCloudPlaceholder
	default:
		return 0, content.ErrRevisionStale
	}
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
	// Omitting FILE_OPEN_REPARSE_POINT while keeping OBJ_DONT_REPARSE admits
	// Cloud Files sync roots/directories and rejects junctions/symlinks in the kernel.
	// FILE_OPEN_NO_RECALL cannot be combined with FILE_DIRECTORY_FILE in NtCreateFile.
	err = windows.NtCreateFile(
		&handle, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
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
	// Combine OBJ_DONT_REPARSE with FILE_OPEN_NO_RECALL (and omit FILE_OPEN_REPARSE_POINT):
	// the kernel rejects intermediate junctions and leaf symlinks while opening
	// hydrated Cloud Files without triggering implicit hydration.
	return windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_NO_RECALL |
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
		return content.WithRevisionComparison(errors.Join(content.ErrSourceBusy, err), content.RevisionComparisonUnavailable)
	case isWindowsCapabilityUnavailable(err):
		return errors.Join(content.ErrUnsupportedStability, err)
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND),
		errors.Is(err, windows.ERROR_CANT_ACCESS_FILE):
		return content.WithRevisionComparison(errors.Join(content.ErrRevisionStale, err), content.RevisionComparisonUnavailable)
	case errors.Is(err, windows.ERROR_REPARSE), errors.Is(err, windows.ERROR_REPARSE_OBJECT),
		errors.Is(err, windows.ERROR_REPARSE_POINT_ENCOUNTERED):
		return content.WithRevisionComparison(errors.Join(content.ErrRevisionStale, err), content.RevisionComparisonMismatch)
	default:
		return err
	}
}

func classifyWindowsIdentityError(err error) error {
	if err == nil {
		return nil
	}
	if isWindowsCapabilityUnavailable(err) {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return err
}

func classifyWindowsRootOpenError(err error) error {
	if isWindowsCapabilityUnavailable(err) {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return err
}

func isWindowsCapabilityUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}
