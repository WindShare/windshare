//go:build windows

package osfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsV3OutputFilesystem                   = "NTFS"
	windowsV3OutputVolumeNameGUID       uint32  = 1
	windowsV3FileSupportsPOSIXSemantics         = 0x00000400
	windowsV3FileRenameInformationEx            = 65
	windowsV3FileOpened                 uintptr = 1
	windowsV3FileCreated                uintptr = 2
	windowsV3DirectoryAddFile                   = windows.FILE_WRITE_DATA
	windowsV3DirectoryAddSubdirectory           = windows.FILE_APPEND_DATA
	windowsV3DirectoryDeleteChild               = 0x00000040
	windowsV3FileAllAccess                      = 0x001F01FF
	windowsV3MaximumComponentUTF16Units         = 255
	// NT UNICODE_STRING stores byte length in uint16 and reserves one UTF-16
	// unit for the terminator used by NewNTUnicodeString.
	windowsV3MaximumNTNameUTF16Units = (1 << 15) - 2
	windowsV3CloudAttributeMask      = windows.FILE_ATTRIBUTE_REPARSE_POINT |
		windows.FILE_ATTRIBUTE_OFFLINE | 0x00040000 | 0x00400000 // RECALL_ON_OPEN | RECALL_ON_DATA_ACCESS
)

var (
	errWindowsV3OutputUnsupported = errors.New("osfs: Windows v3 output backend is unsupported")
	errWindowsV3OutputUnsafe      = errors.New("osfs: Windows v3 output namespace is unsafe")
	errWindowsV3OutputCollision   = errors.New("osfs: Windows v3 output name already exists")
	errWindowsV3OutputLockBusy    = errors.New("osfs: Windows v3 output lock is already held")
)

// windowsV3OutputError retains a machine-readable failure category without
// making callers parse a path-bearing diagnostic. The v3 integration layer can
// consequently map admission, quarantine, collision, and contention decisions
// without weakening this native boundary.
type windowsV3OutputError struct {
	Operation string
	Path      string
	Category  error
	Cause     error
}

func (failure *windowsV3OutputError) Error() string {
	detail := ""
	if failure.Category != nil {
		detail = failure.Category.Error()
	}
	if failure.Cause != nil && detail != "" {
		detail += ": " + failure.Cause.Error()
	} else if failure.Cause != nil {
		detail = failure.Cause.Error()
	}
	if detail == "" {
		detail = "operation failed without a classified cause"
	}
	if failure.Path == "" {
		return fmt.Sprintf("%s: %s", failure.Operation, detail)
	}
	return fmt.Sprintf("%s %q: %s", failure.Operation, failure.Path, detail)
}

func (failure *windowsV3OutputError) Is(target error) bool {
	return failure.Category != nil && errors.Is(failure.Category, target)
}

func (failure *windowsV3OutputError) Unwrap() error { return failure.Cause }

func windowsV3Failure(operation, path string, category, cause error) error {
	return &windowsV3OutputError{Operation: operation, Path: path, Category: category, Cause: cause}
}

// Operational failures retain diagnostic context without inventing namespace
// evidence. Callers can therefore pause and retry raw policy or transient
// denials, while explicit semantic categories continue to drive collision,
// unsupported-platform, and quarantine decisions.
func windowsV3OperationalFailure(operation, path string, cause error) error {
	return &windowsV3OutputError{Operation: operation, Path: path, Cause: cause}
}

func windowsV3NativeOperationFailure(operation, path string, cause error) error {
	if windowsV3IsUnsupportedNative(cause) {
		return windowsV3Failure(operation, path, errWindowsV3OutputUnsupported, cause)
	}
	return windowsV3OperationalFailure(operation, path, cause)
}

func windowsV3NativeNoReplaceFailure(operation, path string, cause error) error {
	if windowsV3IsCollision(cause) {
		return windowsV3Failure(operation, path, errWindowsV3OutputCollision, cause)
	}
	return windowsV3NativeOperationFailure(operation, path, cause)
}

type windowsV3OutputDurability uint8

const windowsV3OutputProcessRestart windowsV3OutputDurability = 1

type windowsV3VolumeIdentity struct {
	guid   string
	serial uint64
}

func (identity windowsV3VolumeIdentity) valid() bool {
	return identity.guid != "" && identity.serial != 0
}

type windowsV3ObjectIdentity struct {
	volume windowsV3VolumeIdentity
	fileID [16]byte
}

func (identity windowsV3ObjectIdentity) valid() bool {
	return identity.volume.valid() && identity.fileID != [16]byte{}
}

// The identity deliberately has no byte/string encoding. It is only a token
// for comparing two simultaneously open objects; the hard-link anchor, not a
// persisted File ID, is the durable ownership witness.
func (identity windowsV3ObjectIdentity) same(other windowsV3ObjectIdentity) bool {
	return identity.valid() && identity == other
}

type windowsV3HandleFacts struct {
	filesystem    string
	path          string
	driveType     uint32
	flags         uint32
	attributes    uint32
	caseSensitive bool
	object        windowsV3ObjectIdentity
}

type windowsV3HandleInspector interface {
	Inspect(windows.Handle) (windowsV3HandleFacts, error)
}

type windowsV3HandleInspectorFunc func(windows.Handle) (windowsV3HandleFacts, error)

func (inspect windowsV3HandleInspectorFunc) Inspect(handle windows.Handle) (windowsV3HandleFacts, error) {
	return inspect(handle)
}

type nativeWindowsV3HandleInspector struct{}

type windowsV3FileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func (nativeWindowsV3HandleInspector) Inspect(handle windows.Handle) (windowsV3HandleFacts, error) {
	var filesystem [32]uint16
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(
		handle, nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem)),
	); err != nil {
		return windowsV3HandleFacts{}, err
	}

	path, err := windowsV3FinalPath(handle, 0)
	if err != nil {
		return windowsV3HandleFacts{}, err
	}
	volumePath, err := windowsV3VolumePath(path)
	if err != nil {
		return windowsV3HandleFacts{}, err
	}
	volumeGUIDPath, err := windowsV3FinalPath(handle, windowsV3OutputVolumeNameGUID)
	if err != nil {
		return windowsV3HandleFacts{}, err
	}
	volumeGUID := strings.ToLower(filepath.VolumeName(volumeGUIDPath))
	if volumeGUID == "" {
		return windowsV3HandleFacts{}, errors.New("Windows volume GUID path has no volume name")
	}

	var fileID windowsV3FileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return windowsV3HandleFacts{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsV3HandleFacts{}, err
	}
	caseSensitive := false
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		var sensitivity struct{ Flags uint32 }
		if err := windows.GetFileInformationByHandleEx(
			handle,
			windows.FileCaseSensitiveInfo,
			(*byte)(unsafe.Pointer(&sensitivity)),
			uint32(unsafe.Sizeof(sensitivity)),
		); err != nil {
			return windowsV3HandleFacts{}, err
		}
		caseSensitive = sensitivity.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0
	}
	return windowsV3HandleFacts{
		filesystem:    windows.UTF16ToString(filesystem[:]),
		path:          path,
		driveType:     windows.GetDriveType(&volumePath[0]),
		flags:         flags,
		attributes:    information.FileAttributes,
		caseSensitive: caseSensitive,
		object: windowsV3ObjectIdentity{
			volume: windowsV3VolumeIdentity{guid: volumeGUID, serial: fileID.VolumeSerialNumber},
			fileID: fileID.FileID,
		},
	}, nil
}

func windowsV3FinalPath(handle windows.Handle, flags uint32) (string, error) {
	buffer := make([]uint16, 512)
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

func windowsV3VolumePath(path string) ([]uint16, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	buffer := make([]uint16, 512)
	if err := windows.GetVolumePathName(encoded, &buffer[0], uint32(len(buffer))); err != nil {
		return nil, err
	}
	return buffer, nil
}

func validateWindowsV3Certification(facts windowsV3HandleFacts) error {
	if err := validateWindowsV3VolumeCertification(facts); err != nil {
		return err
	}
	if err := validateWindowsV3DirectoryShape(facts, "certify output root", "output root"); err != nil {
		return err
	}
	if facts.caseSensitive {
		return windowsV3Failure("certify output root", facts.path, errWindowsV3OutputUnsupported,
			errors.New("case-sensitive NTFS output directories are not certified"))
	}
	return nil
}

func validateWindowsV3ExternalPlacement(
	facts windowsV3HandleFacts,
	expected windowsV3VolumeIdentity,
) error {
	if err := validateWindowsV3VolumeCertification(facts); err != nil {
		return err
	}
	if facts.object.volume != expected {
		return windowsV3Failure("certify external output placement", facts.path, errWindowsV3OutputUnsafe,
			errors.New("external placement crossed the certified NTFS volume boundary"))
	}
	// External components are spelling/placement authorities, not output lookup
	// roots. Per-directory case-sensitive lookup therefore does not affect the
	// handle-bound output namespace beneath them.
	return validateWindowsV3DirectoryShape(
		facts, "certify external output placement", "external output placement",
	)
}

func validateWindowsV3VolumeCertification(facts windowsV3HandleFacts) error {
	switch {
	case !strings.EqualFold(facts.filesystem, windowsV3OutputFilesystem):
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			fmt.Errorf("filesystem %q is not NTFS", facts.filesystem))
	case strings.HasPrefix(strings.TrimPrefix(facts.path, `\\?\`), `UNC\`):
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("network filesystems are not certified"))
	case facts.driveType != windows.DRIVE_FIXED:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			fmt.Errorf("drive type %d is not a certified fixed disk", facts.driveType))
	case facts.flags&windows.FILE_SUPPORTS_HARD_LINKS == 0:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume does not report regular-file hard-link support"))
	case facts.flags&windows.FILE_PERSISTENT_ACLS == 0:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume does not report persistent ACL support"))
	case facts.flags&windowsV3FileSupportsPOSIXSemantics == 0:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume does not report handle-bound rename/unlink support"))
	default:
		return nil
	}
}

func validateWindowsV3DirectoryShape(
	facts windowsV3HandleFacts,
	operation string,
	role string,
) error {
	switch {
	case facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return windowsV3Failure(operation, facts.path, errWindowsV3OutputUnsupported,
			errors.New(role+" is not a directory"))
	case facts.attributes&windowsV3CloudAttributeMask != 0:
		return windowsV3Failure(operation, facts.path, errWindowsV3OutputUnsupported,
			errors.New("reparse, offline, and cloud-placeholder directories are not certified"))
	case !facts.object.valid():
		return windowsV3Failure(operation, facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume or File ID identity is unavailable"))
	default:
		return nil
	}
}

type windowsV3PrivatePolicy struct {
	userSID             *windows.SID
	systemSID           *windows.SID
	administratorsSID   *windows.SID
	trustedInstallerSID *windows.SID
}

const windowsV3TrustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

func newWindowsV3PrivatePolicy() (*windowsV3PrivatePolicy, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Copy the token-owned SID so the policy cannot outlive its backing token
	// information buffer.
	userSID, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return nil, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	administratorsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	trustedInstallerSID, err := windows.StringToSid(windowsV3TrustedInstallerSID)
	if err != nil {
		return nil, err
	}
	return &windowsV3PrivatePolicy{
		userSID: userSID, systemSID: systemSID,
		administratorsSID: administratorsSID, trustedInstallerSID: trustedInstallerSID,
	}, nil
}

func (policy *windowsV3PrivatePolicy) descriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	if policy == nil || policy.userSID == nil || policy.systemSID == nil {
		return nil, errors.New("Windows private ACL policy is unavailable")
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	entries := fmt.Sprintf("(A;%s;GA;;;%s)", inheritance, policy.userSID.String())
	if !policy.userSID.Equals(policy.systemSID) {
		entries += fmt.Sprintf("(A;%s;GA;;;%s)", inheritance, policy.systemSID.String())
	}
	// P protects the DACL from parent inheritance. Only the effective user and
	// LocalSystem retain access; inherited broad desktop ACLs never enter the
	// recovery namespace.
	return windows.SecurityDescriptorFromString("O:" + policy.userSID.String() + "D:P" + entries)
}

func (policy *windowsV3PrivatePolicy) verify(handle windows.Handle, directory bool) error {
	expectedFlags := uint8(0)
	if directory {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	return policy.verifyObjectPolicy(
		handle, windows.SE_FILE_OBJECT, windowsV3FileAllAccess, expectedFlags,
	)
}

func (policy *windowsV3PrivatePolicy) verifyKernelMutex(handle windows.Handle) error {
	return policy.verifyObjectPolicy(handle, windows.SE_KERNEL_OBJECT, windows.MUTEX_ALL_ACCESS, 0)
}

func (policy *windowsV3PrivatePolicy) verifyObjectPolicy(
	handle windows.Handle,
	objectType windows.SE_OBJECT_TYPE,
	expectedMask windows.ACCESS_MASK,
	expectedFlags uint8,
) error {
	if policy == nil || policy.userSID == nil || policy.systemSID == nil {
		return errors.New("Windows private ACL policy is unavailable")
	}
	descriptor, err := windows.GetSecurityInfo(
		handle, objectType, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("private DACL is not protected from inheritance")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(policy.userSID) {
		return errors.Join(errors.New("private object owner differs from the effective user"), err)
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || defaulted {
		return errors.Join(errors.New("private object DACL is absent or defaulted"), err)
	}

	expectedCount := uint16(2)
	if policy.userSID.Equals(policy.systemSID) {
		expectedCount = 1
	}
	if dacl.AceCount != expectedCount {
		return fmt.Errorf("private DACL contains %d entries; expected %d", dacl.AceCount, expectedCount)
	}
	userFound, systemFound := false, policy.userSID.Equals(policy.systemSID)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags != expectedFlags || ace.Mask != expectedMask {
			if ace == nil {
				return errors.New("private DACL contains a nil access entry")
			}
			return fmt.Errorf("private DACL access entry type=%d flags=%#x mask=%#x; expected type=%d flags=%#x mask=%#x",
				ace.Header.AceType, ace.Header.AceFlags, ace.Mask,
				windows.ACCESS_ALLOWED_ACE_TYPE, expectedFlags, expectedMask)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(policy.userSID) && !userFound:
			userFound = true
		case sid.Equals(policy.systemSID) && !systemFound:
			systemFound = true
		default:
			return errors.New("private DACL grants an unexpected or duplicate principal")
		}
	}
	if !userFound || !systemFound {
		return errors.New("private DACL omits a required principal")
	}
	return nil
}

type windowsV3OutputPlatform struct {
	root       *windowsV3Directory
	inspector  windowsV3HandleInspector
	policy     *windowsV3PrivatePolicy
	durability windowsV3OutputDurability
}

func openWindowsV3OutputPlatform(path string) (*windowsV3OutputPlatform, error) {
	return openWindowsV3OutputPlatformWithInspector(path, nativeWindowsV3HandleInspector{})
}

func openWindowsV3OutputPlatformWithInspector(path string, inspector windowsV3HandleInspector) (*windowsV3OutputPlatform, error) {
	if inspector == nil || path == "" {
		return nil, windowsV3Failure("open output root", path, errWindowsV3OutputUnsupported, errors.New("missing root or inspector"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, windowsV3Failure("resolve output root", path, errWindowsV3OutputUnsupported, err)
	}
	handle, _, err := windowsV3OpenNativeWithOptions(
		0, windowsV3NTPath(absolute), windowsV3RootDirectoryAccess(), windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE, 0, nil,
		// The root is the containment anchor rather than one of its descendants.
		// Keeping delete sharing here permits private probe/control children to be
		// reduced while public child handles still pin their own placement.
		windowsV3DirectoryShareMode(false),
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	if err != nil {
		return nil, windowsV3NativeOperationFailure("open output root", absolute, err)
	}
	file := os.NewFile(uintptr(handle), absolute)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, windowsV3Failure("open output root", absolute, errWindowsV3OutputUnsupported, errors.New("wrap root handle"))
	}
	facts, err := inspector.Inspect(handle)
	if err != nil {
		err = windowsV3Failure("inspect output root", absolute, errWindowsV3OutputUnsupported, err)
	} else {
		err = validateWindowsV3Certification(facts)
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		return nil, errors.Join(windowsV3Failure("prepare private output ACL", absolute, errWindowsV3OutputUnsupported, err), file.Close())
	}
	root := &windowsV3Directory{
		file: file, path: absolute, volume: facts.object.volume,
		objectIDs: nativeWindowsV3PersistentObjectIDProvider{}, inspector: inspector, policy: policy,
		objectIDState:     newWindowsV3PersistentObjectIDState(),
		ancestryAuthority: windowsV3NativeAncestryAuthorityVerifier{policy: policy},
		enumerate:         &sync.Mutex{}, placementGuard: true,
	}
	return &windowsV3OutputPlatform{
		root: root, inspector: inspector, policy: policy, durability: windowsV3OutputProcessRestart,
	}, nil
}

func (platform *windowsV3OutputPlatform) Durability() windowsV3OutputDurability {
	if platform == nil {
		return 0
	}
	return platform.durability
}

func (platform *windowsV3OutputPlatform) Root() *windowsV3Directory {
	if platform == nil {
		return nil
	}
	return platform.root
}

func (platform *windowsV3OutputPlatform) Close() error {
	if platform == nil || platform.root == nil {
		return nil
	}
	err := platform.root.Close()
	platform.root = nil
	return err
}

type windowsV3Directory struct {
	file               *os.File
	path               string
	volume             windowsV3VolumeIdentity
	objectIDs          windowsV3PersistentObjectIDProvider
	objectIDState      *windowsV3PersistentObjectIDState
	inspector          windowsV3HandleInspector
	policy             *windowsV3PrivatePolicy
	ancestryAuthority  windowsV3AncestryAuthorityVerifier
	enumerate          *sync.Mutex
	createObserver     windowsV3PrivateDirectoryCreateObserver
	private            bool
	placementGuard     bool
	selfPlacementGuard bool
}

func (directory *windowsV3Directory) handle() windows.Handle {
	return windows.Handle(directory.file.Fd())
}

func (directory *windowsV3Directory) Close() error {
	if directory == nil || directory.file == nil {
		return nil
	}
	err := directory.file.Close()
	directory.file = nil
	return err
}

func (directory *windowsV3Directory) OpenDirectory(relative string) (*windowsV3Directory, error) {
	return directory.openDirectory(relative, directory.private, windows.FILE_OPEN)
}

func (directory *windowsV3Directory) OpenPrivateDirectory(relative string) (*windowsV3Directory, error) {
	return directory.openDirectory(relative, true, windows.FILE_OPEN)
}

func (directory *windowsV3Directory) CreatePrivateDirectory(relative string) (*windowsV3Directory, error) {
	return directory.createPrivateDirectory(relative)
}

func (directory *windowsV3Directory) OpenOrCreatePrivateDirectory(relative string) (*windowsV3Directory, bool, error) {
	opened, err := directory.OpenPrivateDirectory(relative)
	if err == nil {
		return opened, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	created, err := directory.CreatePrivateDirectory(relative)
	if !errors.Is(err, errWindowsV3OutputCollision) {
		return created, err == nil, err
	}
	opened, err = directory.OpenPrivateDirectory(relative)
	return opened, false, err
}

func (directory *windowsV3Directory) openDirectory(relative string, private bool, disposition uint32) (*windowsV3Directory, error) {
	opened, _, err := directory.openDirectoryStatus(relative, private, disposition)
	return opened, err
}

func (directory *windowsV3Directory) openDirectoryStatus(relative string, private bool, disposition uint32) (*windowsV3Directory, bool, error) {
	if err := directory.usable(); err != nil {
		return nil, false, err
	}
	if private && disposition != windows.FILE_OPEN {
		return nil, false, windowsV3Failure(
			"create private output directory", relative, errWindowsV3OutputUnsafe,
			errors.New("private mutation requires the crash-safe delete-on-close commit protocol"),
		)
	}
	native, err := windowsV3RelativePath(relative, private)
	if err != nil {
		return nil, false, windowsV3Failure("open output directory", relative, errWindowsV3OutputUnsafe, err)
	}
	var descriptor *windows.SECURITY_DESCRIPTOR
	attributes := uint32(0)
	if private {
		descriptor, err = directory.policy.descriptor(true)
		if err != nil {
			return nil, false, windowsV3Failure("prepare private directory ACL", relative, errWindowsV3OutputUnsafe, err)
		}
		attributes = windows.FILE_ATTRIBUTE_HIDDEN
	}
	// Public output directories carry locator authority, so their open handles
	// deny delete sharing for the complete operation. A private child disables
	// that guard for its whole subtree because recovery must rename and remove
	// those entries through concurrently retained identity witnesses.
	placementGuard := directory.placementGuard && !private
	handle, status, err := windowsV3OpenNativeWithOptions(
		directory.handle(), native, windowsV3OpenedDirectoryAccess(placementGuard), disposition,
		windows.FILE_DIRECTORY_FILE, attributes, descriptor,
		windowsV3DirectoryShareMode(placementGuard),
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		if disposition == windows.FILE_CREATE {
			return nil, false, windowsV3NativeNoReplaceFailure("open output directory", relative, err)
		}
		return nil, false, windowsV3NativeOperationFailure("open output directory", relative, err)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, false, windowsV3Failure("open output directory", relative, errWindowsV3OutputUnsafe, errors.New("wrap directory handle"))
	}
	opened := &windowsV3Directory{
		file: file, path: filepath.Join(directory.path, relative), volume: directory.volume,
		objectIDs: directory.objectIDs, inspector: directory.inspector, policy: directory.policy,
		objectIDState: newWindowsV3PersistentObjectIDState(), ancestryAuthority: directory.ancestryAuthority,
		enumerate: &sync.Mutex{}, createObserver: directory.createObserver, private: private,
		placementGuard: placementGuard, selfPlacementGuard: placementGuard,
	}
	if err := opened.verify(private); err != nil {
		return nil, false, errors.Join(err, opened.Close())
	}
	if err := windowsV3VerifyOpenedLeafAuthority(opened.handle(), native, private); err != nil {
		return nil, false, errors.Join(err, opened.Close())
	}
	created, err := windowsV3CreationStatus(disposition, status)
	if err != nil {
		return nil, false, errors.Join(windowsV3Failure("classify output directory open", relative, errWindowsV3OutputUnsafe, err), opened.Close())
	}
	if private {
		if _, err := opened.preparePrivatePersistentObjectID(); err != nil {
			return nil, false, errors.Join(err, opened.Close())
		}
	}
	return opened, created, nil
}

func (directory *windowsV3Directory) verify(private bool) error {
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return windowsV3Failure("inspect output directory", directory.path, errWindowsV3OutputUnsafe, err)
	}
	if err := windowsV3ValidateOpenedObject(facts, directory.volume, true); err != nil {
		return windowsV3Failure("inspect output directory", directory.path, errWindowsV3OutputUnsafe, err)
	}
	if private {
		if facts.attributes&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
			return windowsV3Failure("verify private output directory", directory.path, errWindowsV3OutputUnsafe,
				errors.New("directory is not hidden"))
		}
		if err := directory.policy.verify(directory.handle(), true); err != nil {
			return windowsV3Failure("verify private output directory", directory.path, errWindowsV3OutputUnsafe, err)
		}
	}
	return nil
}

func (directory *windowsV3Directory) usable() error {
	if directory == nil || directory.file == nil || directory.inspector == nil || directory.policy == nil {
		return windowsV3Failure("use output directory", "", errWindowsV3OutputUnsafe, errors.New("directory handle is closed or incomplete"))
	}
	return nil
}

func (directory *windowsV3Directory) Sync() error {
	if err := directory.usable(); err != nil {
		return err
	}
	if err := directory.verify(false); err != nil {
		return err
	}
	// The explicit flush orders namespace milestones even though this backend's
	// public claim stops at process restart. It must not be reinterpreted as a
	// power-loss guarantee without fault testing the complete storage stack.
	if err := windows.FlushFileBuffers(directory.handle()); err != nil {
		return windowsV3NativeOperationFailure("sync output directory", directory.path, err)
	}
	return nil
}

type windowsV3File struct {
	file      *os.File
	path      string
	volume    windowsV3VolumeIdentity
	inspector windowsV3HandleInspector
	policy    *windowsV3PrivatePolicy
}

func (file *windowsV3File) handle() windows.Handle { return windows.Handle(file.file.Fd()) }

func (file *windowsV3File) Close() error {
	if file == nil || file.file == nil {
		return nil
	}
	err := file.file.Close()
	file.file = nil
	return err
}

func (file *windowsV3File) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.file == nil {
		return 0, windowsV3Failure("read output file", "", errWindowsV3OutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.ReadAt(destination, offset)
}

func (file *windowsV3File) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.file == nil {
		return 0, windowsV3Failure("write output file", "", errWindowsV3OutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.WriteAt(source, offset)
}

func (file *windowsV3File) Truncate(size int64) error {
	if file == nil || file.file == nil {
		return windowsV3Failure("size output file", "", errWindowsV3OutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.Truncate(size)
}

func (file *windowsV3File) Sync() error {
	if file == nil || file.file == nil {
		return windowsV3Failure("sync output file", "", errWindowsV3OutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.Sync()
}

func (directory *windowsV3Directory) CreatePrivateFile(relative string) (*windowsV3File, error) {
	opened, _, err := directory.openPrivateFile(relative, windows.FILE_CREATE)
	return opened, err
}

func (directory *windowsV3Directory) OpenPrivateFile(relative string) (*windowsV3File, error) {
	opened, _, err := directory.openPrivateFile(relative, windows.FILE_OPEN)
	return opened, err
}

func (directory *windowsV3Directory) openOrCreatePrivateFile(relative string) (*windowsV3File, bool, error) {
	return directory.openPrivateFile(relative, windows.FILE_OPEN_IF)
}

func (directory *windowsV3Directory) openPrivateFile(relative string, disposition uint32) (*windowsV3File, bool, error) {
	descriptor, err := directory.policy.descriptor(false)
	if err != nil {
		return nil, false, windowsV3Failure("prepare private file ACL", relative, errWindowsV3OutputUnsafe, err)
	}
	return directory.openFile(relative, disposition, windowsV3PrivateFileAccess(), descriptor, true)
}

func (directory *windowsV3Directory) OpenRegularFile(relative string) (*windowsV3File, error) {
	opened, _, err := directory.openFile(
		relative, windows.FILE_OPEN, windowsV3ReadFileAccess(), nil, directory.private,
	)
	return opened, err
}

func (directory *windowsV3Directory) openFileForDelete(relative string) (*windowsV3File, error) {
	opened, _, err := directory.openFile(
		relative, windows.FILE_OPEN, windowsV3DeleteFileAccess(), nil, directory.private,
	)
	return opened, err
}

func (directory *windowsV3Directory) openFile(
	relative string,
	disposition uint32,
	access uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
	private bool,
) (*windowsV3File, bool, error) {
	if err := directory.usable(); err != nil {
		return nil, false, err
	}
	native, err := windowsV3RelativePath(relative, private)
	if err != nil {
		return nil, false, windowsV3Failure("open output file", relative, errWindowsV3OutputUnsafe, err)
	}
	handle, status, err := windowsV3OpenNative(
		directory.handle(), native, access, disposition, windows.FILE_NON_DIRECTORY_FILE,
		windows.FILE_ATTRIBUTE_NORMAL, descriptor,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		if disposition == windows.FILE_CREATE {
			return nil, false, windowsV3NativeNoReplaceFailure("open output file", relative, err)
		}
		return nil, false, windowsV3NativeOperationFailure("open output file", relative, err)
	}
	wrapped := os.NewFile(uintptr(handle), relative)
	if wrapped == nil {
		_ = windows.CloseHandle(handle)
		return nil, false, windowsV3Failure("open output file", relative, errWindowsV3OutputUnsafe, errors.New("wrap file handle"))
	}
	file := &windowsV3File{
		file: wrapped, path: filepath.Join(directory.path, relative), volume: directory.volume,
		inspector: directory.inspector, policy: directory.policy,
	}
	if err := file.verify(private); err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	if err := windowsV3VerifyOpenedLeafAuthority(file.handle(), native, private); err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	created, err := windowsV3CreationStatus(disposition, status)
	if err != nil {
		return nil, false, errors.Join(windowsV3Failure("classify output file open", relative, errWindowsV3OutputUnsafe, err), file.Close())
	}
	return file, created, nil
}

func (file *windowsV3File) verify(private bool) error {
	if file == nil || file.file == nil || file.inspector == nil {
		return windowsV3Failure("inspect output file", "", errWindowsV3OutputUnsafe, errors.New("file handle is closed or incomplete"))
	}
	facts, err := file.inspector.Inspect(file.handle())
	if err != nil {
		return windowsV3Failure("inspect output file", file.path, errWindowsV3OutputUnsafe, err)
	}
	if err := windowsV3ValidateOpenedObject(facts, file.volume, false); err != nil {
		return windowsV3Failure("inspect output file", file.path, errWindowsV3OutputUnsafe, err)
	}
	if private {
		if err := file.policy.verify(file.handle(), false); err != nil {
			return windowsV3Failure("verify private output file", file.path, errWindowsV3OutputUnsafe, err)
		}
	}
	return nil
}

func windowsV3ValidateOpenedObject(facts windowsV3HandleFacts, expected windowsV3VolumeIdentity, directory bool) error {
	if !strings.EqualFold(facts.filesystem, windowsV3OutputFilesystem) || facts.object.volume != expected {
		return errors.New("opened object crossed the certified NTFS volume boundary")
	}
	if facts.attributes&windowsV3CloudAttributeMask != 0 {
		return errors.New("opened object is a reparse, offline, or cloud-placeholder object")
	}
	isDirectory := facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		return errors.New("opened object has the wrong file type")
	}
	if directory && facts.caseSensitive {
		return errors.New("opened directory enables case-sensitive lookup")
	}
	if !facts.object.valid() {
		return errors.New("opened object has no current File ID identity")
	}
	return nil
}

func sameWindowsV3OpenedObject(left, right *windowsV3File) (bool, error) {
	if left == nil || right == nil || left.file == nil || right.file == nil || left.inspector == nil || right.inspector == nil {
		return false, windowsV3Failure("compare output objects", "", errWindowsV3OutputUnsafe, errors.New("missing open file handle"))
	}
	leftFacts, leftErr := left.inspector.Inspect(left.handle())
	rightFacts, rightErr := right.inspector.Inspect(right.handle())
	if leftErr != nil || rightErr != nil {
		return false, windowsV3Failure("compare output objects", "", errWindowsV3OutputUnsafe, errors.Join(leftErr, rightErr))
	}
	return leftFacts.object.same(rightFacts.object), nil
}

func sameWindowsV3OpenedDirectory(left, right *windowsV3Directory) (bool, error) {
	if left == nil || right == nil || left.file == nil || right.file == nil || left.inspector == nil || right.inspector == nil {
		return false, windowsV3Failure("compare output directories", "", errWindowsV3OutputUnsafe, errors.New("missing open directory handle"))
	}
	leftFacts, leftErr := left.inspector.Inspect(left.handle())
	rightFacts, rightErr := right.inspector.Inspect(right.handle())
	if leftErr != nil || rightErr != nil {
		return false, windowsV3Failure("compare output directories", "", errWindowsV3OutputUnsafe, errors.Join(leftErr, rightErr))
	}
	return leftFacts.object.same(rightFacts.object), nil
}

func (directory *windowsV3Directory) LinkRegularFileNoReplace(source *windowsV3File, target string) (*windowsV3File, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if source == nil || source.file == nil || source.volume != directory.volume {
		return nil, windowsV3Failure("link output file", target, errWindowsV3OutputUnsafe, errors.New("source is absent or on another volume"))
	}
	name, err := windowsV3RelativePath(target, true)
	if err != nil {
		return nil, windowsV3Failure("link output file", target, errWindowsV3OutputUnsafe, err)
	}
	buffer, err := windowsV3LinkRenameBuffer(0, directory.handle(), name)
	if err != nil {
		return nil, windowsV3Failure("link output file", target, errWindowsV3OutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsV3NTError(windows.NtSetInformationFile(
		source.handle(), &status, &buffer[0], uint32(len(buffer)), windows.FileLinkInformation,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(source)
	if err != nil {
		return nil, windowsV3NativeNoReplaceFailure("link output file", target, err)
	}
	linked, err := directory.OpenRegularFile(target)
	if err != nil {
		return nil, windowsV3Failure("verify linked output file", target, errWindowsV3OutputUnsafe, err)
	}
	same, err := sameWindowsV3OpenedObject(source, linked)
	if err != nil || !same {
		return nil, errors.Join(windowsV3Failure("verify linked output file", target, errWindowsV3OutputUnsafe,
			errors.New("destination does not identify the source object")), err, linked.Close())
	}
	return linked, nil
}

func (directory *windowsV3Directory) AtomicReplacePrivateFile(source *windowsV3File, target string) error {
	if err := directory.usable(); err != nil {
		return err
	}
	if source == nil || source.file == nil || source.volume != directory.volume {
		return windowsV3Failure("replace private state", target, errWindowsV3OutputUnsafe, errors.New("source is absent or on another volume"))
	}
	name, err := windowsV3RelativePath(target, true)
	if err != nil {
		return windowsV3Failure("replace private state", target, errWindowsV3OutputUnsafe, err)
	}
	flags := uint32(windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS |
		windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE)
	buffer, err := windowsV3LinkRenameBuffer(flags, directory.handle(), name)
	if err != nil {
		return windowsV3Failure("replace private state", target, errWindowsV3OutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsV3NTError(windows.NtSetInformationFile(
		source.handle(), &status, &buffer[0], uint32(len(buffer)), windowsV3FileRenameInformationEx,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(source)
	if err != nil {
		return windowsV3NativeOperationFailure("replace private state", target, err)
	}
	installed, err := directory.OpenPrivateFile(target)
	if err != nil {
		return windowsV3Failure("verify replaced private state", target, errWindowsV3OutputUnsafe, err)
	}
	same, compareErr := sameWindowsV3OpenedObject(source, installed)
	closeErr := installed.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(windowsV3Failure("verify replaced private state", target, errWindowsV3OutputUnsafe,
			errors.New("installed state does not identify the source object")), compareErr, closeErr)
	}
	return nil
}

func (directory *windowsV3Directory) InstallPrivateDirectoryNoReplace(source *windowsV3Directory, target string) (*windowsV3Directory, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if source == nil || source.file == nil || source.volume != directory.volume {
		return nil, windowsV3Failure("install private directory", target, errWindowsV3OutputUnsafe,
			errors.New("source is absent or on another volume"))
	}
	name, err := windowsV3RelativePath(target, true)
	if err != nil {
		return nil, windowsV3Failure("install private directory", target, errWindowsV3OutputUnsafe, err)
	}
	buffer, err := windowsV3LinkRenameBuffer(windows.FILE_RENAME_POSIX_SEMANTICS, directory.handle(), name)
	if err != nil {
		return nil, windowsV3Failure("install private directory", target, errWindowsV3OutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsV3NTError(windows.NtSetInformationFile(
		source.handle(), &status, &buffer[0], uint32(len(buffer)), windowsV3FileRenameInformationEx,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(source)
	if err != nil {
		switch {
		case windowsV3IsCollision(err):
			return nil, windowsV3NativeNoReplaceFailure("install private directory", target, err)
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			// NTFS reports ACCESS_DENIED, rather than NAME_COLLISION, for a
			// no-replace directory rename whose target is present. Resolve only
			// the already-failed target entry through the pinned parent; a safe
			// current directory proves collision only after its observation
			// handle is settled, while any ambiguous/reparse observation remains
			// unsafe.
			existing, openErr := directory.OpenPrivateDirectory(target)
			var closeErr error
			if existing != nil {
				closeErr = existing.Close()
			}
			return nil, windowsV3DirectoryInstallDeniedFailure(target, err, openErr, closeErr)
		default:
			return nil, windowsV3NativeNoReplaceFailure("install private directory", target, err)
		}
	}
	installed, err := directory.OpenPrivateDirectory(target)
	if err != nil {
		return nil, windowsV3Failure("verify installed private directory", target, errWindowsV3OutputUnsafe, err)
	}
	same, compareErr := sameWindowsV3OpenedDirectory(source, installed)
	if compareErr != nil || !same {
		return nil, errors.Join(windowsV3Failure("verify installed private directory", target, errWindowsV3OutputUnsafe,
			errors.New("installed directory does not identify the fixed candidate")), compareErr, installed.Close())
	}
	return installed, nil
}

func windowsV3DirectoryInstallDeniedFailure(target string, installErr, observationErr, closeErr error) error {
	if closeErr != nil {
		// A close failure leaves the handle lifecycle unsettled. Treating the
		// preceding observation as a clean collision would let bootstrap skip
		// the failure solely because it recognizes the collision category.
		return windowsV3OperationalFailure(
			"close private directory collision observation", target,
			errors.Join(installErr, observationErr, closeErr),
		)
	}
	if observationErr == nil {
		return windowsV3Failure("install private directory", target, errWindowsV3OutputCollision, installErr)
	}
	if errors.Is(observationErr, errWindowsV3OutputUnsafe) {
		return windowsV3Failure(
			"install private directory", target, errWindowsV3OutputUnsafe,
			errors.Join(installErr, observationErr),
		)
	}
	return windowsV3NativeOperationFailure(
		"install private directory", target, errors.Join(installErr, observationErr),
	)
}

type windowsV3LinkRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func windowsV3LinkRenameBuffer(flags uint32, root windows.Handle, name string) ([]byte, error) {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return nil, err
	}
	encoded = encoded[:len(encoded)-1]
	var layout windowsV3LinkRenameInformation
	headerSize := int(unsafe.Offsetof(layout.FileName))
	buffer := make([]byte, headerSize+len(encoded)*2)
	information := (*windowsV3LinkRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.Flags = flags
	information.RootDirectory = root
	information.FileNameLength = uint32(len(encoded) * 2)
	copy(unsafe.Slice(&information.FileName[0], len(encoded)), encoded)
	return buffer, nil
}

func (directory *windowsV3Directory) RemoveRegularLink(relative string, expected *windowsV3File) error {
	current, err := directory.openFileForDelete(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	same, compareErr := sameWindowsV3OpenedObject(current, expected)
	if compareErr != nil || !same {
		return errors.Join(windowsV3Failure("remove output link", relative, errWindowsV3OutputUnsafe,
			errors.New("current directory entry differs from the expected open object")), compareErr, current.Close())
	}
	removeErr := windowsV3RemoveHandle(current.handle())
	return errors.Join(removeErr, current.Close())
}

func (directory *windowsV3Directory) RemoveDirectory(relative string, expected *windowsV3Directory) error {
	// The child's authority decides its security and placement policy. A private
	// probe directly beneath the public root must remain movable for cleanup,
	// while an arbitrary public child must never be silently downgraded.
	private := directory.private
	if expected != nil {
		private = expected.private
	}
	current, err := directory.openDirectory(relative, private, windows.FILE_OPEN)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	same, compareErr := sameWindowsV3OpenedDirectory(current, expected)
	if compareErr != nil || !same {
		return errors.Join(windowsV3Failure("remove output directory", relative, errWindowsV3OutputUnsafe,
			errors.New("current directory entry differs from the expected open directory")), compareErr, current.Close())
	}
	removeErr := windowsV3RemoveHandle(current.handle())
	return errors.Join(removeErr, current.Close())
}

type windowsV3DispositionInformation struct{ Flags uint32 }

func windowsV3RemoveHandle(handle windows.Handle) error {
	information := windowsV3DispositionInformation{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	err := windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		return windowsV3NativeOperationFailure("remove opened output object", "", err)
	}
	return nil
}

type windowsV3StableLock struct {
	mu         sync.Mutex
	file       *windowsV3File
	overlapped windows.Overlapped
	closed     bool
}

func (directory *windowsV3Directory) AcquireStableLock(relative string) (*windowsV3StableLock, bool, error) {
	file, created, err := directory.openOrCreatePrivateFile(relative)
	if err != nil {
		return nil, false, err
	}
	lock, err := windowsV3LockStableFile(file, relative)
	return lock, created, err
}

func (directory *windowsV3Directory) AcquireExistingStableLock(relative string) (*windowsV3StableLock, error) {
	file, err := directory.OpenPrivateFile(relative)
	if err != nil {
		return nil, err
	}
	lock, err := windowsV3LockStableFile(file, relative)
	return lock, err
}

func windowsV3LockStableFile(file *windowsV3File, relative string) (*windowsV3StableLock, error) {
	lock := &windowsV3StableLock{file: file}
	err := windows.LockFileEx(
		file.handle(), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &lock.overlapped,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, errors.Join(windowsV3Failure(
				"lock stable output authority", relative, errWindowsV3OutputLockBusy, err,
			), file.Close())
		}
		return nil, errors.Join(windowsV3NativeOperationFailure(
			"lock stable output authority", relative, err,
		), file.Close())
	}
	return lock, nil
}

func (lock *windowsV3StableLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	if lock.file == nil || lock.file.file == nil {
		return nil
	}
	unlockErr := windows.UnlockFileEx(lock.file.handle(), 0, 1, 0, &lock.overlapped)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}

func windowsV3OpenNative(
	root windows.Handle,
	name string,
	access uint32,
	disposition uint32,
	typeOption uint32,
	attributes uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, uintptr, error) {
	return windowsV3OpenNativeWithOptions(
		root,
		name,
		access,
		disposition,
		typeOption,
		attributes,
		descriptor,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
}

func windowsV3OpenNativeWithOptions(
	root windows.Handle,
	name string,
	access uint32,
	disposition uint32,
	typeOption uint32,
	attributes uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
	shareMode uint32,
	objectAttributes uint32,
) (windows.Handle, uintptr, error) {
	nativeName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, 0, err
	}
	object := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: nativeName, Attributes: objectAttributes,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	createOptions := typeOption | windows.FILE_OPEN_REPARSE_POINT
	if access&windows.SYNCHRONIZE != 0 {
		createOptions |= windows.FILE_SYNCHRONOUS_IO_NONALERT
	}
	err = windows.NtCreateFile(
		&handle, access, object, &status, nil, attributes,
		shareMode,
		disposition, createOptions,
		0, 0,
	)
	runtime.KeepAlive(descriptor)
	return handle, status.Information, normalizeWindowsV3NTError(err)
}

func windowsV3NTPath(path string) string {
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

func normalizeWindowsV3NTError(err error) error {
	if err == nil {
		return nil
	}
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		return status.Errno()
	}
	return err
}

func windowsV3DirectoryAccess() uint32 {
	return windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windowsV3DirectoryAddFile |
		windowsV3DirectoryAddSubdirectory | windowsV3DirectoryDeleteChild | windows.FILE_READ_ATTRIBUTES |
		windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE
}

func windowsV3OpenedDirectoryAccess(placementGuard bool) uint32 {
	if placementGuard {
		return windowsV3RootDirectoryAccess()
	}
	return windowsV3DirectoryAccess()
}

func windowsV3DirectoryShareMode(placementGuard bool) uint32 {
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if !placementGuard {
		share |= windows.FILE_SHARE_DELETE
	}
	return share
}

func windowsV3RootDirectoryAccess() uint32 {
	// DELETE is needed on child handles that are renamed or retired, but asking
	// for it on the output root makes unrelated Win32 directory readers share
	// deletion unnecessarily. FILE_DELETE_CHILD is the authority needed to
	// mutate entries beneath the pinned root.
	return windowsV3DirectoryAccess() &^ windows.DELETE
}

func windowsV3PrivateFileAccess() uint32 {
	return windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES |
		windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE
}

func windowsV3ReadFileAccess() uint32 {
	return windows.FILE_GENERIC_READ | windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsV3DeleteFileAccess() uint32 {
	return windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsV3CreationStatus(disposition uint32, status uintptr) (bool, error) {
	switch disposition {
	case windows.FILE_CREATE:
		if status != windowsV3FileCreated {
			return false, fmt.Errorf("exclusive create returned status %d", status)
		}
		return true, nil
	case windows.FILE_OPEN:
		if status != windowsV3FileOpened {
			return false, fmt.Errorf("open returned status %d", status)
		}
		return false, nil
	case windows.FILE_OPEN_IF:
		switch status {
		case windowsV3FileCreated:
			return true, nil
		case windowsV3FileOpened:
			return false, nil
		default:
			return false, fmt.Errorf("open-or-create returned status %d", status)
		}
	default:
		return false, fmt.Errorf("unsupported create disposition %d", disposition)
	}
}

func windowsV3RelativePath(path string, leafOnly bool) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || strings.Contains(path, ":") {
		return "", errors.New("empty, NUL, and alternate-stream paths are forbidden")
	}
	native := filepath.FromSlash(path)
	if !filepath.IsLocal(native) || filepath.IsAbs(native) || native == "." || filepath.Clean(native) != native {
		return "", errors.New("path is not a canonical root-relative name")
	}
	if len(utf16.Encode([]rune(native))) > windowsV3MaximumNTNameUTF16Units {
		return "", errors.New("path exceeds the NT object-name UTF-16 limit")
	}
	components := strings.Split(native, string(filepath.Separator))
	if leafOnly && len(components) != 1 {
		return "", errors.New("operation requires one directory-entry name")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return "", errors.New("path component has an unsafe Win32 alias")
		}
		if len(utf16.Encode([]rune(component))) > windowsV3MaximumComponentUTF16Units {
			return "", errors.New("path component exceeds the certified NTFS UTF-16 limit")
		}
		if windowsV3ReservedComponent(component) {
			return "", errors.New("path component is reserved or not representable through Win32")
		}
	}
	return native, nil
}

func windowsV3ReservedComponent(component string) bool {
	for _, character := range component {
		if character < 0x20 || strings.ContainsRune(`<>"|?*`, character) {
			return true
		}
	}
	base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		"COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	default:
		return false
	}
}

func windowsV3IsCollision(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func windowsV3IsUnsupportedNative(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}

var _ io.ReaderAt = (*windowsV3File)(nil)
var _ io.WriterAt = (*windowsV3File)(nil)
