//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	errWindowsV3OutputUnsupported = errors.New("osfs: windows v3 output backend is unsupported")
	errWindowsV3OutputUnsafe      = errors.New("osfs: windows v3 output namespace is unsafe")
	errWindowsV3OutputCollision   = errors.New("osfs: windows v3 output name already exists")
	errWindowsV3OutputLockBusy    = errors.New("osfs: windows v3 output lock is already held")
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
		return windowsV3HandleFacts{}, errors.New("windows volume GUID path has no volume name")
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
		return nil, errors.New("windows private ACL policy is unavailable")
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
		return errors.New("windows private ACL policy is unavailable")
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
