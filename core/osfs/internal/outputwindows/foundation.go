//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
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

func (platform *windowsOutputV3Platform) Root() outputcap.Directory {
	if platform == nil || platform.native == nil || platform.root == nil || platform.root.native == nil {
		return nil
	}
	return platform.root
}

func (platform *windowsOutputV3Platform) RootOpenDisposition() outputcap.RootOpenDisposition {
	if platform == nil {
		return ""
	}
	return platform.rootOpenDisposition
}

func (platform *windowsOutputV3Platform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
			windowsOutputV3Error(guard.Close()),
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	return &windowsOutputV3PublicOperationGuard{
		native: guard,
		root:   &windowsOutputV3Directory{native: root},
	}, nil
}

func (guard *windowsOutputV3PublicOperationGuard) Root() outputcap.Directory {
	if guard == nil || guard.native == nil || guard.root == nil || guard.root.native == nil {
		return nil
	}
	return guard.root
}

func (guard *windowsOutputV3PublicOperationGuard) Close() error {
	if guard == nil || guard.native == nil {
		return nil
	}
	err := guard.native.Close()
	guard.native = nil
	if guard.root != nil {
		guard.root.native = nil
	}
	guard.root = nil
	return windowsOutputV3Error(err)
}

func (*windowsOutputV3Platform) Certification() outputcap.CertificationID {
	return outputcap.CertificationWindowsNTFSProcessRestart
}

func (platform *windowsOutputV3Platform) RootBinding() (
	_ outputcap.OutputRootBinding,
	resultErr error,
) {
	if platform == nil || platform.native == nil || platform.native.root == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows output platform is closed"),
		)
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputV3Error(err)
	}
	defer func() { resultErr = errors.Join(resultErr, windowsOutputV3Error(guard.Close())) }()
	root := guard.Root()
	if root == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	if _, err := root.prepareIdentityClaim(); err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputV3Error(err)
	}
	facts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputV3Error(
			windowsV3Failure("bind output root", root.path, errWindowsV3OutputUnsafe, err),
		)
	}
	if err := windowsV3ValidateOpenedObject(facts, root.volume, true); err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputV3Error(
			windowsV3Failure("bind output root", root.path, errWindowsV3OutputUnsafe, err),
		)
	}
	objectID, prepared := root.objectIDState.current()
	if !prepared {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows output-root Object ID was not prepared"),
		)
	}
	guid := strings.ToLower(facts.object.volume.guid)
	if len(guid) == 0 || len(guid) > windowsV3VolumeGUIDClaimMaxBytes {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows volume GUID identity exceeds the bounded root-binding format"),
		)
	}
	volume := make([]byte, len("windows/ntfs/volume/v1")+4+len(guid)+8)
	copy(volume, "windows/ntfs/volume/v1")
	offset := len("windows/ntfs/volume/v1")
	binary.BigEndian.PutUint32(volume[offset:], uint32(len(guid)))
	offset += 4
	copy(volume[offset:], guid)
	offset += len(guid)
	binary.BigEndian.PutUint64(volume[offset:], facts.object.volume.serial)

	object := make([]byte, len("windows/ntfs/directory-object/v2")+len(objectID))
	copy(object, "windows/ntfs/directory-object/v2")
	copy(object[len("windows/ntfs/directory-object/v2"):], objectID[:])
	binding, err := outputcap.NewOutputRootBinding(platform.Certification(), volume, object)
	return binding, windowsOutputV3Error(err)
}

func (*windowsOutputV3Platform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}

func (*windowsOutputV3Platform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return checkpointmodel.LiveCleanupWindowsNTFSV1
}

func (platform *windowsOutputV3Platform) DestinationCapabilities() (
	_ outputcap.DestinationCapabilities,
	resultErr error,
) {
	if platform == nil || platform.native == nil || platform.native.root == nil {
		return outputcap.DestinationCapabilities{}, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	var guard *windowsV3PublicOperationGuard
	var err error
	if platform.root.native.private {
		guard, err = platform.native.acquirePrivatePublicationRootGuard()
	} else {
		guard, err = platform.native.acquirePublicOperationGuard()
	}
	if err != nil {
		return outputcap.DestinationCapabilities{}, windowsOutputV3Error(err)
	}
	defer func() { resultErr = errors.Join(resultErr, windowsOutputV3Error(guard.Close())) }()
	return guard.Root().destinationCapabilities()
}

func (platform *windowsOutputV3Platform) ProbeRecoverableFeatures() error {
	capabilities, err := platform.DestinationCapabilities()
	if err != nil {
		return err
	}
	if mode, modeErr := outputcap.SelectExecutionMode(capabilities); modeErr != nil ||
		mode != outputcap.ExecutionResumable {
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, modeErr)
	}
	return nil
}

func (*windowsOutputV3Platform) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	return windowsOutputV3Error(windowsV3ValidateModifiedTime(modified))
}

func (*windowsOutputV3Platform) CanonicalLocatorKey(path string) (string, error) {
	key, err := windowsV3OutputLocatorKey(path)
	return key, windowsOutputV3Error(err)
}

func (*windowsOutputV3Platform) CanonicalComponentKey(name string) (string, error) {
	native, err := windowsV3RelativePath(name, true)
	if err != nil {
		return "", windowsOutputV3Error(err)
	}
	key, err := windowsV3NTFSCaseKey(native)
	return key, windowsOutputV3Error(err)
}

func (platform *windowsOutputV3Platform) Close() error {
	if platform == nil || platform.native == nil {
		return nil
	}
	var err error
	if platform.publicationGuard != nil {
		err = errors.Join(err, platform.publicationGuard.Close())
		platform.publicationGuard = nil
	}
	err = errors.Join(err, platform.native.Close())
	platform.native = nil
	if platform.root != nil {
		platform.root.native = nil
	}
	platform.root = nil
	return windowsOutputV3Error(err)
}
