//go:build windows

package osfs

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
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
)

var (
	modCldAPI                                 = windows.NewLazySystemDLL("cldapi.dll")
	procCfGetPlaceholderStateFromAttributeTag = modCldAPI.NewProc("CfGetPlaceholderStateFromAttributeTag")
)

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

func queryWindowsPlaceholderState(attributes, reparseTag uint32) uint32 {
	if procCfGetPlaceholderStateFromAttributeTag.Find() != nil {
		return cfPlaceholderStateInvalid
	}
	state, _, _ := procCfGetPlaceholderStateFromAttributeTag.Call(uintptr(attributes), uintptr(reparseTag))
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
	if facts.mode != 0 && !facts.mode.IsRegular() {
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

func queryWindowsPathAttributeTagNoRecall(absolute string) (windowsAttributeTagInfo, uint32, error) {
	path16, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	// Open only the leaf object's metadata without following leaf reparse points
	// or recalling cloud data; ancestor junctions remain traversable for selected roots.
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_OPEN_NO_RECALL)
	handle, err := windows.CreateFile(
		path16, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0,
	)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	defer windows.CloseHandle(handle)
	tagInfo, err := queryWindowsHandleAttributeTag(handle)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	return tagInfo, fileType, nil
}

func queryWindowsRelativeAttributeTagNoRecall(parent *os.File, name string) (windowsAttributeTagInfo, uint32, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_NO_RECALL|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, normalizeWindowsNTError(err)
	}
	defer windows.CloseHandle(handle)
	tagInfo, err := queryWindowsHandleAttributeTag(handle)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return windowsAttributeTagInfo{}, 0, err
	}
	return tagInfo, fileType, nil
}

func openRootObjectNoRecall(root *os.Root, name string) (*os.File, error) {
	// Go's Windows os.Root.OpenFile forwards FILE_FLAG_OPEN_NO_RECALL to NtCreateFile
	// alongside OBJ_DONT_REPARSE (without FILE_OPEN_REPARSE_POINT), allowing hydrated
	// cloud directories and files while preventing implicit data recall on open.
	return root.OpenFile(name, os.O_RDONLY|windows.FILE_FLAG_OPEN_NO_RECALL, 0)
}

func inspectSelectedRootKind(absolute string) (fs.FileInfo, sourceEntryKind, sourceEntryAudit, error) {
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, 0, sourceEntryAudit{}, err
	}
	attrs, hasAttrs := windowsFileInfoAttributes(before)
	if !hasAttrs || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		kind, audit := classifyWindowsEntry(windowsEntryFacts{
			source:        windowsAttributesFromHandle,
			mode:          before.Mode(),
			attributes:    attrs,
			hasAttributes: hasAttrs,
		})
		return before, kind, audit, nil
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return before, sourceEntrySymbolicLink, sourceEntryAudit{attributes: attrs, reparseTag: windows.IO_REPARSE_TAG_SYMLINK}, nil
	}
	tagInfo, fileType, err := queryWindowsPathAttributeTagNoRecall(absolute)
	if err != nil {
		return nil, 0, sourceEntryAudit{}, err
	}
	var state uint32
	if isCloudReparseTag(tagInfo.ReparseTag) {
		state = queryWindowsPlaceholderState(tagInfo.FileAttributes, tagInfo.ReparseTag)
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source:           windowsAttributesFromHandle,
		mode:             before.Mode(),
		attributes:       tagInfo.FileAttributes,
		hasAttributes:    true,
		hasReparseTag:    true,
		reparseTag:       tagInfo.ReparseTag,
		placeholderState: state,
		hasFileType:      true,
		fileType:         fileType,
	})
	return before, kind, audit, nil
}

func inspectDirEntryPreflight(entry fs.DirEntry) (fs.FileInfo, sourceEntryKind, sourceEntryAudit, bool, bool) {
	enumInfo, err := entry.Info()
	if err != nil {
		return nil, 0, sourceEntryAudit{}, false, false
	}
	enumAttrs, ok := windowsFileInfoAttributes(enumInfo)
	if !ok {
		return nil, 0, sourceEntryAudit{}, false, false
	}
	recallOnOpen := enumAttrs&windowsAttrRecallOnOpen != 0
	// Catch non-reparse cloud placeholders (including disguise mode and
	// directory-only RECALL_ON_OPEN) before opening any child handle.
	if enumAttrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, 0, sourceEntryAudit{}, false, recallOnOpen
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source:        windowsAttributesFromDirectoryEnum,
		mode:          enumInfo.Mode(),
		attributes:    enumAttrs,
		hasAttributes: true,
	})
	if omissionForEntryKind(kind) != catalog.SourceOmissionNone {
		return enumInfo, kind, audit, true, recallOnOpen
	}
	return nil, 0, sourceEntryAudit{}, false, recallOnOpen
}

func inspectRootChildKind(
	directory *os.Root,
	dirHandle *os.File,
	entry fs.DirEntry,
) (fs.FileInfo, sourceEntryKind, sourceEntryAudit, error) {
	enumInfo, enumKind, enumAudit, omitted, dirRecallOnOpen := inspectDirEntryPreflight(entry)
	if omitted {
		return enumInfo, enumKind, enumAudit, nil
	}
	name := entry.Name()
	before, err := directory.Lstat(name)
	if err != nil {
		return nil, 0, sourceEntryAudit{}, err
	}
	attrs, hasAttrs := windowsFileInfoAttributes(before)
	if !hasAttrs || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		kind, audit := classifyWindowsEntry(windowsEntryFacts{
			source:        windowsAttributesFromHandle,
			mode:          before.Mode(),
			attributes:    attrs,
			hasAttributes: hasAttrs,
		})
		if dirRecallOnOpen && omissionForEntryKind(kind) == catalog.SourceOmissionNone {
			return before, sourceEntryCloudPlaceholder, sourceEntryAudit{attributes: attrs | windowsAttrRecallOnOpen}, nil
		}
		return before, kind, audit, nil
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return before, sourceEntrySymbolicLink, sourceEntryAudit{attributes: attrs, reparseTag: windows.IO_REPARSE_TAG_SYMLINK}, nil
	}
	tagInfo, fileType, err := queryWindowsRelativeAttributeTagNoRecall(dirHandle, name)
	if err != nil {
		return nil, 0, sourceEntryAudit{}, err
	}
	var state uint32
	if isCloudReparseTag(tagInfo.ReparseTag) {
		state = queryWindowsPlaceholderState(tagInfo.FileAttributes, tagInfo.ReparseTag)
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source:           windowsAttributesFromHandle,
		mode:             before.Mode(),
		attributes:       tagInfo.FileAttributes,
		hasAttributes:    true,
		hasReparseTag:    true,
		reparseTag:       tagInfo.ReparseTag,
		placeholderState: state,
		hasFileType:      true,
		fileType:         fileType,
	})
	if dirRecallOnOpen && audit.hydratedCloud {
		audit.hydratedCloud = false
		return before, sourceEntryCloudPlaceholder, audit, nil
	}
	return before, kind, audit, nil
}

func inspectHandleKind(handle *os.File, info fs.FileInfo) (sourceEntryKind, sourceEntryAudit, error) {
	attrs, hasAttrs := windowsFileInfoAttributes(info)
	if !hasAttrs || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		kind, audit := classifyWindowsEntry(windowsEntryFacts{
			source:        windowsAttributesFromHandle,
			mode:          info.Mode(),
			attributes:    attrs,
			hasAttributes: hasAttrs,
		})
		return kind, audit, nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return sourceEntrySymbolicLink, sourceEntryAudit{attributes: attrs, reparseTag: windows.IO_REPARSE_TAG_SYMLINK}, nil
	}
	raw := windows.Handle(handle.Fd())
	tagInfo, err := queryWindowsHandleAttributeTag(raw)
	if err != nil {
		return 0, sourceEntryAudit{}, err
	}
	var state uint32
	if isCloudReparseTag(tagInfo.ReparseTag) {
		state = queryWindowsPlaceholderState(tagInfo.FileAttributes, tagInfo.ReparseTag)
	}
	facts := windowsEntryFacts{
		source:           windowsAttributesFromHandle,
		mode:             info.Mode(),
		attributes:       tagInfo.FileAttributes,
		hasAttributes:    true,
		hasReparseTag:    true,
		reparseTag:       tagInfo.ReparseTag,
		placeholderState: state,
	}
	kind, audit := classifyWindowsEntry(facts)
	if kind == sourceEntryRegularFile && audit.hydratedCloud {
		fileType, ftErr := windows.GetFileType(raw)
		if ftErr != nil {
			return 0, sourceEntryAudit{}, ftErr
		}
		if fileType != windows.FILE_TYPE_DISK {
			audit.hydratedCloud = false
			return sourceEntrySpecialFile, audit, nil
		}
	}
	return kind, audit, nil
}

// canOpenRootRelativeRegularFile performs a zero-recall preflight check on
// Root.Lstat metadata before OpenStable opens the handle. Reparse points that
// are non-directory, non-symlink, and non-offline proceed to the no-recall open
// where inspectHandleKind verifies the exact tag, placeholder state, and file type.
func canOpenRootRelativeRegularFile(info fs.FileInfo) bool {
	if info == nil || info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	attrs, hasAttrs := windowsFileInfoAttributes(info)
	if hasAttrs {
		if attrs&(windows.FILE_ATTRIBUTE_DIRECTORY|windowsAttrOffline|windowsAttrRecallOnDataAccess) != 0 {
			return false
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return info.Mode()&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket) == 0
		}
	}
	return info.Mode().IsRegular()
}

func isWindowsPlaceholderError(err error) bool {
	return errors.Is(err, errSourceCloudPlaceholder)
}
