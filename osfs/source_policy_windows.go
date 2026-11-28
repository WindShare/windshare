//go:build windows

package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/windows"
)

const (
	ioReparseTagCloud          uint32 = 0x9000001A
	ioReparseTagCloudMask      uint32 = 0x0000F000
	reparseTagNameSurrogateBit uint32 = 0x20000000

	// FILE_ATTRIBUTE_RECALL_ON_OPEN shares bit 0x00040000 with FILE_ATTRIBUTE_EA.
	// It denotes recall semantics only in directory enumeration records, never in
	// GetFileAttributesW or handle-based FileAttributeTagInfo / ByHandleFileInformation.
	windowsAttrRecallOnOpen       uint32 = 0x00040000
	windowsAttrEA                 uint32 = windowsAttrRecallOnOpen
	windowsAttrRecallOnDataAccess uint32 = 0x00400000
	windowsAttrOffline            uint32 = 0x00001000

	cfPlaceholderStatePlaceholder     uint32 = 0x00000001
	cfPlaceholderStateSyncRoot        uint32 = 0x00000002
	cfPlaceholderStateEssentialProp   uint32 = 0x00000004
	cfPlaceholderStateInSync          uint32 = 0x00000008
	cfPlaceholderStatePartial         uint32 = 0x00000010
	cfPlaceholderStatePartiallyOnDisk uint32 = 0x00000020
	cfPlaceholderStateKnownMask       uint32 = cfPlaceholderStatePlaceholder |
		cfPlaceholderStateSyncRoot | cfPlaceholderStateEssentialProp |
		cfPlaceholderStateInSync | cfPlaceholderStatePartial | cfPlaceholderStatePartiallyOnDisk
	cfPlaceholderStateInvalid uint32 = 0xFFFFFFFF

	windowsShareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

	initialFinalPathUnits               = 512
	windowsExtendedPrefix               = `\\?\`
	windowsExtendedUNCPrefix            = `\\?\UNC\`
	windowsNTNamespacePrefix            = `\??\`
	windowsDeviceNamespacePrefix        = `\\.\`
	windowsVolumeNameDOS         uint32 = 0x0
	windowsVolumeNameGUID        uint32 = 0x1
)

var (
	modCldAPI                                 = windows.NewLazySystemDLL("cldapi.dll")
	procCfGetPlaceholderStateFromAttributeTag = modCldAPI.NewProc("CfGetPlaceholderStateFromAttributeTag")
)

type sourceObjectIdentity struct {
	volumeSerial  uint32
	fileIndexHigh uint32
	fileIndexLow  uint32
	isDir         bool
}

func (identity sourceObjectIdentity) sameObject(other sourceObjectIdentity) bool {
	return identity == other
}

// rootInspector retains no-recall root handles for metadata-only relative
// preflight because Go 1.27's Windows os.Root.Lstat opens with FILE_GENERIC_READ
// and without FILE_OPEN_NO_RECALL.
type rootInspector struct {
	handles []windows.Handle
}

func newRootInspector(roots []*os.Root) (rootInspector, error) {
	handles := make([]windows.Handle, 0, len(roots))
	for _, root := range roots {
		handle, err := duplicateRootHandleNoRecall(root)
		if err != nil {
			return rootInspector{}, errors.Join(err, closeWindowsHandles(handles))
		}
		handles = append(handles, handle)
	}
	return rootInspector{handles: handles}, nil
}

func (inspector rootInspector) Close() error {
	return closeWindowsHandles(inspector.handles)
}

func (inspector rootInspector) inspectRelativeRegularFile(
	_ *os.Root,
	slot int,
	relative string,
) (sourceObjectIdentity, bool, error) {
	if slot < 0 || slot >= len(inspector.handles) {
		return sourceObjectIdentity{}, false, content.ErrRevisionStale
	}
	identity, kind, _, err := inspectWindowsRelativeNoRecall(inspector.handles[slot], relative, false)
	if err != nil {
		return sourceObjectIdentity{}, false, err
	}
	return identity, kind == sourceEntryRegularFile, nil
}

func duplicateRootHandleNoRecall(authority *os.Root) (windows.Handle, error) {
	file, err := openRootObjectNoRecall(authority, ".")
	if err != nil {
		return windows.InvalidHandle, err
	}
	var handle windows.Handle
	process := windows.CurrentProcess()
	duplicateErr := windows.DuplicateHandle(
		process,
		windows.Handle(file.Fd()),
		process,
		&handle,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	)
	runtime.KeepAlive(file)
	closeErr := file.Close()
	if err := errors.Join(duplicateErr, closeErr); err != nil {
		if duplicateErr == nil {
			err = errors.Join(err, windows.CloseHandle(handle))
		}
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func closeWindowsHandles(handles []windows.Handle) error {
	var result error
	for index := range slices.Backward(handles) {
		handle := handles[index]
		if handle == 0 || handle == windows.InvalidHandle {
			continue
		}
		result = errors.Join(result, windows.CloseHandle(handle))
	}
	return result
}

type windowsAttributeSource uint8

const (
	windowsAttributesFromDirectoryEnum windowsAttributeSource = iota + 1
	windowsAttributesFromHandle
)

type windowsEntryFacts struct {
	source           windowsAttributeSource
	mode             fs.FileMode
	attributes       uint32
	hasAttributes    bool
	hasReparseTag    bool
	reparseTag       uint32
	placeholderState uint32
	hasFileType      bool
	fileType         uint32
}

type windowsAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

func isCloudReparseTag(tag uint32) bool {
	return tag&reparseTagNameSurrogateBit == 0 && (tag&^ioReparseTagCloudMask) == ioReparseTagCloud
}

// queryWindowsHandlePlaceholderState decodes Cloud Files state from handle-based
// FileAttributeTagInfo. cldapi.dll unconditionally treats bit 0x00040000 as
// FILE_ATTRIBUTE_RECALL_ON_OPEN (reporting CF_PLACEHOLDER_STATE_PARTIAL), whereas
// handle queries use that bit for NTFS FILE_ATTRIBUTE_EA.
func queryWindowsHandlePlaceholderState(attributes, reparseTag uint32) uint32 {
	if procCfGetPlaceholderStateFromAttributeTag.Find() != nil {
		return cfPlaceholderStateInvalid
	}
	state, _, _ := procCfGetPlaceholderStateFromAttributeTag.Call(
		uintptr(attributes&^windowsAttrEA),
		uintptr(reparseTag),
	)
	return uint32(state)
}

func windowsRecallAttributeMask(source windowsAttributeSource) uint32 {
	mask := windowsAttrOffline | windowsAttrRecallOnDataAccess
	if source == windowsAttributesFromDirectoryEnum {
		mask |= windowsAttrRecallOnOpen
	}
	return mask
}

// classifyWindowsEntry is the single authoritative admission classifier for
// Windows roots, scanned children, and open handles. Unknown tags or
// unrecognized placeholder states fail closed.
func classifyWindowsEntry(facts windowsEntryFacts) (sourceEntryKind, sourceEntryAudit) {
	if facts.mode&fs.ModeSymlink != 0 {
		return sourceEntrySymbolicLink, sourceEntryAudit{attributes: facts.attributes, reparseTag: facts.reparseTag}
	}
	recallMask := windowsRecallAttributeMask(facts.source)
	if facts.hasAttributes && facts.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return classifyWindowsReparseEntry(facts, recallMask)
	}
	if facts.hasAttributes && facts.attributes&recallMask != 0 {
		return sourceEntryCloudPlaceholder, sourceEntryAudit{attributes: facts.attributes}
	}
	if (facts.hasAttributes && facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) || facts.mode.IsDir() {
		return sourceEntryDirectory, sourceEntryAudit{}
	}
	if facts.hasFileType && facts.fileType != windows.FILE_TYPE_DISK {
		return sourceEntrySpecialFile, sourceEntryAudit{}
	}
	// Strip Go's ModeIrregular when authoritative handle fileType is present so
	// a cloud file that converted to an ordinary local file between queries is
	// not misclassified as a special file.
	mode := facts.mode
	if facts.hasFileType {
		mode &^= fs.ModeIrregular
	}
	if mode != 0 && !mode.IsRegular() {
		return sourceEntrySpecialFile, sourceEntryAudit{}
	}
	return sourceEntryRegularFile, sourceEntryAudit{}
}

func classifyWindowsReparseEntry(facts windowsEntryFacts, recallMask uint32) (sourceEntryKind, sourceEntryAudit) {
	audit := sourceEntryAudit{attributes: facts.attributes, reparseTag: facts.reparseTag}
	if !facts.hasReparseTag {
		return sourceEntryReparsePoint, audit
	}
	if facts.reparseTag == windows.IO_REPARSE_TAG_SYMLINK {
		return sourceEntrySymbolicLink, audit
	}
	if !isCloudReparseTag(facts.reparseTag) {
		return sourceEntryReparsePoint, audit
	}
	audit.placeholderState = facts.placeholderState
	audit.hasCloudState = true
	if facts.placeholderState == cfPlaceholderStateInvalid ||
		facts.placeholderState&cfPlaceholderStatePlaceholder == 0 ||
		facts.placeholderState&^cfPlaceholderStateKnownMask != 0 {
		return sourceEntryReparsePoint, audit
	}
	if facts.placeholderState&(cfPlaceholderStatePartial|cfPlaceholderStatePartiallyOnDisk) != 0 ||
		facts.attributes&recallMask != 0 {
		return sourceEntryCloudPlaceholder, audit
	}
	if facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		audit.hydratedCloud = true
		return sourceEntryDirectory, audit
	}
	if facts.hasFileType && facts.fileType != windows.FILE_TYPE_DISK {
		return sourceEntrySpecialFile, audit
	}
	if facts.mode&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 {
		return sourceEntrySpecialFile, audit
	}
	audit.hydratedCloud = true
	return sourceEntryRegularFile, audit
}

func windowsFileInfoAttributes(info fs.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	native, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || native == nil {
		return 0, false
	}
	return native.FileAttributes, true
}

func windowsExtendedPath(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, windowsExtendedPrefix) ||
		strings.HasPrefix(path, windowsNTNamespacePrefix) ||
		strings.HasPrefix(path, windowsDeviceNamespacePrefix) {
		return path
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, windowsExtendedPrefix) ||
		strings.HasPrefix(clean, windowsNTNamespacePrefix) ||
		strings.HasPrefix(clean, windowsDeviceNamespacePrefix) {
		return clean
	}
	if rest, ok := strings.CutPrefix(clean, `\\`); ok {
		return windowsExtendedUNCPrefix + rest
	}
	if len(clean) >= 3 && clean[1] == ':' && clean[2] == '\\' {
		return windowsExtendedPrefix + clean
	}
	return path
}

func queryWindowsHandleAttributeTag(handle windows.Handle) (windowsAttributeTagInfo, error) {
	var info windowsAttributeTagInfo
	err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	return info, err
}

func classifyWindowsOpenHandle(
	handle windows.Handle,
	dirRecallOnOpen bool,
	queryFileTypeUpfront bool,
) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, windows.ByHandleFileInformation, error) {
	var fileType uint32
	hasFileType := false
	if queryFileTypeUpfront {
		ft, err := windows.GetFileType(handle)
		if err != nil {
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, windows.ByHandleFileInformation{}, err
		}
		if ft == windows.FILE_TYPE_PIPE || ft == windows.FILE_TYPE_CHAR {
			return sourceObjectIdentity{}, sourceEntrySpecialFile, sourceEntryAudit{}, windows.ByHandleFileInformation{}, nil
		}
		fileType = ft
		hasFileType = true
	}
	var byHandle windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &byHandle); err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, windows.ByHandleFileInformation{}, err
	}
	identity := sourceObjectIdentity{
		volumeSerial:  byHandle.VolumeSerialNumber,
		fileIndexHigh: byHandle.FileIndexHigh,
		fileIndexLow:  byHandle.FileIndexLow,
		isDir:         byHandle.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
	}
	if byHandle.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		kind, audit := classifyWindowsEntry(windowsEntryFacts{
			source:        windowsAttributesFromHandle,
			attributes:    byHandle.FileAttributes,
			hasAttributes: true,
			hasFileType:   hasFileType,
			fileType:      fileType,
		})
		if dirRecallOnOpen && omissionForEntryKind(kind) == catalog.SourceOmissionNone {
			return identity, sourceEntryCloudPlaceholder, sourceEntryAudit{
				attributes: byHandle.FileAttributes | windowsAttrRecallOnOpen,
			}, byHandle, nil
		}
		return identity, kind, audit, byHandle, nil
	}
	tagInfo, err := queryWindowsHandleAttributeTag(handle)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, windows.ByHandleFileInformation{}, err
	}
	hasReparseTag := tagInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	var state uint32
	if hasReparseTag && isCloudReparseTag(tagInfo.ReparseTag) {
		state = queryWindowsHandlePlaceholderState(tagInfo.FileAttributes, tagInfo.ReparseTag)
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source:           windowsAttributesFromHandle,
		attributes:       tagInfo.FileAttributes,
		hasAttributes:    true,
		hasReparseTag:    hasReparseTag,
		reparseTag:       tagInfo.ReparseTag,
		placeholderState: state,
		hasFileType:      hasFileType,
		fileType:         fileType,
	})
	if kind == sourceEntryRegularFile && audit.hydratedCloud && !hasFileType {
		ft, ftErr := windows.GetFileType(handle)
		if ftErr != nil {
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, windows.ByHandleFileInformation{}, ftErr
		}
		if ft != windows.FILE_TYPE_DISK {
			audit.hydratedCloud = false
			return identity, sourceEntrySpecialFile, audit, byHandle, nil
		}
	}
	if dirRecallOnOpen && omissionForEntryKind(kind) == catalog.SourceOmissionNone {
		audit.attributes = tagInfo.FileAttributes | windowsAttrRecallOnOpen
		audit.hydratedCloud = false
		return identity, sourceEntryCloudPlaceholder, audit, byHandle, nil
	}
	return identity, kind, audit, byHandle, nil
}

func inspectWindowsRelativeNoRecall(
	parent windows.Handle,
	relative string,
	dirRecallOnOpen bool,
) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
	if parent == 0 || parent == windows.InvalidHandle {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, os.ErrInvalid
	}
	normalized := filepath.FromSlash(relative)
	if !filepath.IsLocal(normalized) || filepath.IsAbs(normalized) {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, content.ErrRevisionStale
	}
	objectName, err := windows.NewNTUnicodeString(normalized)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes, &status, nil, 0,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_NO_RECALL|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, normalizeWindowsNTError(err)
	}
	defer windows.CloseHandle(handle)
	identity, kind, audit, _, err := classifyWindowsOpenHandle(handle, dirRecallOnOpen, false)
	return identity, kind, audit, err
}

func inspectWindowsChildNoRecall(
	dirHandle *os.File,
	name string,
	dirRecallOnOpen bool,
) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
	if dirHandle == nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, os.ErrInvalid
	}
	defer runtime.KeepAlive(dirHandle)
	return inspectWindowsRelativeNoRecall(windows.Handle(dirHandle.Fd()), name, dirRecallOnOpen)
}

func openRootObjectNoRecall(root *os.Root, name string) (*os.File, error) {
	// Go's Windows os.Root.OpenFile forwards FILE_FLAG_OPEN_NO_RECALL to NtCreateFile
	// alongside OBJ_DONT_REPARSE (without FILE_OPEN_REPARSE_POINT), allowing hydrated
	// cloud directories and files while preventing implicit data recall on open.
	return root.OpenFile(name, os.O_RDONLY|windows.FILE_FLAG_OPEN_NO_RECALL, 0)
}

func inspectSelectedRootKind(absolute string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
	path16, err := windows.UTF16PtrFromString(windowsExtendedPath(absolute))
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, err
	}
	// Open only the leaf object's metadata without following leaf reparse points
	// or recalling cloud data; ancestor junctions remain traversable for selected roots.
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_OPEN_NO_RECALL)
	handle, err := windows.CreateFile(
		path16, 0,
		windowsShareAll,
		nil, windows.OPEN_EXISTING, flags, 0,
	)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, err
	}
	defer windows.CloseHandle(handle)
	identity, kind, audit, _, err := classifyWindowsOpenHandle(handle, false, true)
	return identity, kind, audit, err
}

func isLinkedDirectoryLeaf(kind sourceEntryKind, audit sourceEntryAudit) bool {
	return kind == sourceEntrySymbolicLink ||
		(kind == sourceEntryReparsePoint && audit.reparseTag == windows.IO_REPARSE_TAG_MOUNT_POINT)
}

// resolveLinkedWorkingDirectory resolves a pure "." / ".." traversal whose
// logical working directory ends in a Windows symlink or directory junction.
// Opening without FILE_FLAG_OPEN_REPARSE_POINT lets the kernel follow both
// symlinks and mount points while FILE_FLAG_OPEN_NO_RECALL prevents implicit
// cloud hydration; target admission still uses classifyWindowsOpenHandle so
// hydrated cloud directories remain allowed and dehydrated placeholders stay
// rejected.
func resolveLinkedWorkingDirectory(_, absolute string) (resolvedLinkedDirectory, error) {
	return resolveWindowsLinkedDirectoryWith(absolute, openWindowsFollowedTarget, inspectSelectedRootKind)
}

func resolveWindowsLinkedDirectoryWith(
	absolute string,
	follow func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error),
	inspect func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error),
) (resolvedLinkedDirectory, error) {
	identity, kind, audit, resolved, err := follow(absolute)
	if err != nil {
		return resolvedLinkedDirectory{}, err
	}
	switch kind {
	case sourceEntryDirectory:
	case sourceEntryCloudPlaceholder, sourceEntryReparsePoint:
		return resolvedLinkedDirectory{identity: identity, kind: kind, audit: audit}, nil
	default:
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	targetID, targetKind, targetAudit, err := inspect(resolved)
	if err != nil {
		return resolvedLinkedDirectory{}, pathfailure.Filesystem("inspect selected root link target", absolute, err)
	}
	if targetKind == sourceEntryCloudPlaceholder && identity.sameObject(targetID) {
		return resolvedLinkedDirectory{identity: targetID, kind: targetKind, audit: targetAudit}, nil
	}
	if targetKind != sourceEntryDirectory || !identity.sameObject(targetID) {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q does not resolve to the directory the system opens: %w", absolute, content.ErrUnsupportedStability)
	}
	return resolvedLinkedDirectory{
		openPath: resolved,
		identity: targetID,
		kind:     targetKind,
		audit:    targetAudit,
	}, nil
}

func openWindowsFollowedTarget(absolute string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
	path16, err := windows.UTF16PtrFromString(windowsExtendedPath(absolute))
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, "", pathfailure.Filesystem("follow selected root", absolute, err)
	}
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_NO_RECALL)
	handle, err := windows.CreateFile(
		path16,
		windows.FILE_READ_ATTRIBUTES,
		windowsShareAll,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if errors.Is(err, windows.ERROR_CANT_RESOLVE_FILENAME) {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, "", fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, "", pathfailure.Filesystem("follow selected root", absolute, err)
	}
	defer windows.CloseHandle(handle)

	identity, kind, audit, _, err := classifyWindowsOpenHandle(handle, false, true)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, "", pathfailure.Filesystem("inspect selected root link target", absolute, err)
	}
	if kind != sourceEntryDirectory {
		return identity, kind, audit, "", nil
	}
	resolved, err := windowsFinalPathByHandle(handle)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, "", pathfailure.Filesystem("resolve selected root link", absolute, err)
	}
	return identity, kind, audit, resolved, nil
}

func windowsFinalPathByHandle(handle windows.Handle) (string, error) {
	resolved, err := queryWindowsFinalPath(handle, windowsVolumeNameDOS)
	if errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		// Volumes mounted at a junction without an assigned drive letter have no
		// DOS path; fall back to their stable volume GUID path.
		resolved, err = queryWindowsFinalPath(handle, windowsVolumeNameGUID)
	}
	if err != nil {
		return "", err
	}
	return normalizeWindowsFinalPath(resolved), nil
}

func queryWindowsFinalPath(handle windows.Handle, flags uint32) (string, error) {
	buffer := make([]uint16, initialFinalPathUnits)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), flags)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func normalizeWindowsFinalPath(path string) string {
	if rest, ok := strings.CutPrefix(path, windowsExtendedUNCPrefix); ok {
		return `\\` + rest
	}
	if rest, ok := strings.CutPrefix(path, windowsExtendedPrefix); ok &&
		len(rest) >= 3 && rest[1] == ':' && rest[2] == '\\' {
		return rest
	}
	return path
}

func inspectDirEntryPreflight(entry fs.DirEntry) (sourceEntryKind, sourceEntryAudit, bool, bool) {
	enumInfo, err := entry.Info()
	if err != nil {
		return 0, sourceEntryAudit{}, false, false
	}
	enumAttrs, ok := windowsFileInfoAttributes(enumInfo)
	if !ok {
		return 0, sourceEntryAudit{}, false, false
	}
	recallOnOpen := enumAttrs&windowsAttrRecallOnOpen != 0
	if enumInfo.Mode()&fs.ModeSymlink != 0 {
		return sourceEntrySymbolicLink, sourceEntryAudit{
			attributes: enumAttrs,
			reparseTag: windows.IO_REPARSE_TAG_SYMLINK,
		}, true, recallOnOpen
	}
	// Catch non-reparse cloud placeholders (including disguise mode and
	// directory-only RECALL_ON_OPEN) before opening any child handle.
	if enumAttrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0, sourceEntryAudit{}, false, recallOnOpen
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source:        windowsAttributesFromDirectoryEnum,
		mode:          enumInfo.Mode(),
		attributes:    enumAttrs,
		hasAttributes: true,
	})
	if omissionForEntryKind(kind) != catalog.SourceOmissionNone {
		return kind, audit, true, recallOnOpen
	}
	return 0, sourceEntryAudit{}, false, recallOnOpen
}

func inspectRootChildKind(
	_ *os.Root,
	dirHandle *os.File,
	entry fs.DirEntry,
) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
	kind, audit, omitted, dirRecallOnOpen := inspectDirEntryPreflight(entry)
	if omitted {
		return sourceObjectIdentity{}, kind, audit, nil
	}
	return inspectWindowsChildNoRecall(dirHandle, entry.Name(), dirRecallOnOpen)
}

func inspectOpenedSourceHandle(handle *os.File) (openedSourceHandle, error) {
	if handle == nil {
		return openedSourceHandle{}, os.ErrInvalid
	}
	defer runtime.KeepAlive(handle)
	raw := windows.Handle(handle.Fd())
	identity, kind, audit, byHandle, err := classifyWindowsOpenHandle(raw, false, true)
	if err != nil {
		return openedSourceHandle{}, err
	}
	var size uint64
	if kind == sourceEntryRegularFile {
		size = uint64(byHandle.FileSizeHigh)<<32 | uint64(byHandle.FileSizeLow)
	}
	ticks := int64(uint64(byHandle.LastWriteTime.HighDateTime)<<32 | uint64(byHandle.LastWriteTime.LowDateTime))
	modified, err := (windowsMutationToken{lastWrite: ticks}).modifiedTime()
	if err != nil {
		return openedSourceHandle{}, err
	}
	return openedSourceHandle{
		identity: identity,
		kind:     kind,
		audit:    audit,
		size:     size,
		modified: modified,
	}, nil
}

func classifyRootRelativeOpenError(operation, relative string, err error) error {
	switch {
	case errors.Is(err, content.ErrRevisionStale):
		if content.RevisionComparisonOf(err) != content.RevisionComparisonUnavailable {
			return err
		}
		return content.WithRevisionComparison(err, content.RevisionComparisonMismatch)
	case errors.Is(err, os.ErrNotExist), errors.Is(err, windows.ERROR_CANT_ACCESS_FILE):
		return content.WithRevisionComparison(errors.Join(content.ErrRevisionStale, err), content.RevisionComparisonUnavailable)
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return content.WithRevisionComparison(errors.Join(content.ErrSourceBusy, err), content.RevisionComparisonUnavailable)
	case errors.Is(err, windows.ERROR_REPARSE), errors.Is(err, windows.ERROR_REPARSE_OBJECT),
		errors.Is(err, windows.ERROR_REPARSE_POINT_ENCOUNTERED):
		return content.WithRevisionComparison(errors.Join(content.ErrRevisionStale, err), content.RevisionComparisonMismatch)
	default:
		return content.WithRevisionComparison(
			pathfailure.Filesystem(operation, relative, err), content.RevisionComparisonUnavailable,
		)
	}
}

func isTransientRevisionOpenError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func isWindowsPlaceholderError(err error) bool {
	return errors.Is(err, errSourceCloudPlaceholder)
}
