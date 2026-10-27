//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

const (
	windowsOutputFilesystem                   = "NTFS"
	windowsOutputVolumeNameGUID       uint32  = 1
	windowsFileSupportsPOSIXSemantics         = 0x00000400
	windowsFileRenameInformationEx            = 65
	windowsFileOpened                 uintptr = 1
	windowsFileCreated                uintptr = 2
	windowsDirectoryAddFile                   = windows.FILE_WRITE_DATA
	windowsDirectoryAddSubdirectory           = windows.FILE_APPEND_DATA
	windowsDirectoryDeleteChild               = 0x00000040
	windowsFileAllAccess                      = 0x001F01FF
	windowsMaximumComponentUTF16Units         = 255
	// NT UNICODE_STRING stores byte length in uint16 and reserves one UTF-16
	// unit for the terminator used by NewNTUnicodeString.
	windowsMaximumNTNameUTF16Units = (1 << 15) - 2
	windowsCloudAttributeMask      = windows.FILE_ATTRIBUTE_REPARSE_POINT |
		windows.FILE_ATTRIBUTE_OFFLINE | 0x00040000 | 0x00400000 // RECALL_ON_OPEN | RECALL_ON_DATA_ACCESS
)

var (
	errWindowsOutputUnsupported = errors.New("osfs: windows v3 output backend is unsupported")
	errWindowsOutputUnsafe      = errors.New("osfs: windows v3 output namespace is unsafe")
	errWindowsOutputCollision   = errors.New("osfs: windows v3 output name already exists")
	errWindowsOutputLockBusy    = errors.New("osfs: windows v3 output lock is already held")
)

// windowsOutputError retains a machine-readable failure category without
// making callers parse a path-bearing diagnostic. The v3 integration layer can
// consequently map admission, quarantine, collision, and contention decisions
// without weakening this native boundary.
type nativeOutputError struct {
	Operation string
	Path      string
	Category  error
	Cause     error
	native    outputcap.NativeErrorClass
}

func (failure *nativeOutputError) Error() string {
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

func (failure *nativeOutputError) Is(target error) bool {
	return failure.Category != nil && errors.Is(failure.Category, target)
}

func (failure *nativeOutputError) Unwrap() error { return failure.Cause }

func (failure *nativeOutputError) NativeErrorClass() outputcap.NativeErrorClass {
	if failure == nil || !failure.native.Valid() {
		return outputcap.NativeErrorUnknown
	}
	return failure.native
}

func windowsFailure(operation, path string, category, cause error) error {
	return &nativeOutputError{
		Operation: operation,
		Path:      path,
		Category:  category,
		Cause:     cause,
		native:    windowsNativeErrorClass(category, cause),
	}
}

// Operational failures retain diagnostic context without inventing namespace
// evidence. Callers can therefore pause and retry raw policy or transient
// denials, while explicit semantic categories continue to drive collision,
// unsupported-platform, and quarantine decisions.
func windowsOperationalFailure(operation, path string, cause error) error {
	return &nativeOutputError{
		Operation: operation,
		Path:      path,
		Cause:     cause,
		native:    windowsNativeErrorClass(nil, cause),
	}
}

func windowsNativeErrorClass(category, cause error) outputcap.NativeErrorClass {
	switch {
	case errors.Is(cause, windows.ERROR_ACCESS_DENIED),
		errors.Is(cause, windows.ERROR_PRIVILEGE_NOT_HELD):
		return outputcap.NativeErrorAccessDenied
	case errors.Is(cause, windows.ERROR_SHARING_VIOLATION),
		errors.Is(cause, windows.ERROR_LOCK_VIOLATION):
		return outputcap.NativeErrorSharingViolation
	case errors.Is(cause, fs.ErrNotExist),
		errors.Is(cause, windows.ERROR_FILE_NOT_FOUND),
		errors.Is(cause, windows.ERROR_PATH_NOT_FOUND):
		return outputcap.NativeErrorNotFound
	case errors.Is(cause, windows.ERROR_INVALID_HANDLE):
		return outputcap.NativeErrorInvalidHandle
	case errors.Is(category, errWindowsOutputUnsupported), windowsIsUnsupportedNative(cause):
		return outputcap.NativeErrorUnsupported
	}
	if _, ok := errors.AsType[syscall.Errno](cause); ok {
		return outputcap.NativeErrorIO
	}
	return outputcap.NativeErrorUnknown
}

func windowsNativeOperationFailure(operation, path string, cause error) error {
	if windowsIsUnsupportedNative(cause) {
		return windowsFailure(operation, path, errWindowsOutputUnsupported, cause)
	}
	return windowsOperationalFailure(operation, path, cause)
}

func windowsNativeNoReplaceFailure(operation, path string, cause error) error {
	if windowsIsCollision(cause) {
		return windowsFailure(operation, path, errWindowsOutputCollision, cause)
	}
	return windowsNativeOperationFailure(operation, path, cause)
}

type windowsOutputDurability uint8

const windowsOutputProcessRestart windowsOutputDurability = 1

type windowsVolumeIdentity struct {
	guid   string
	serial uint64
}

func (identity windowsVolumeIdentity) valid() bool {
	return identity.guid != "" && identity.serial != 0
}

type windowsObjectIdentity struct {
	volume windowsVolumeIdentity
	fileID [16]byte
}

func (identity windowsObjectIdentity) valid() bool {
	return identity.volume.valid() && identity.fileID != [16]byte{}
}

// The identity deliberately has no byte/string encoding. It is only a token
// for comparing two simultaneously open objects; the hard-link anchor, not a
// persisted File ID, is the durable ownership witness.
func (identity windowsObjectIdentity) same(other windowsObjectIdentity) bool {
	return identity.valid() && identity == other
}

func (platform *windowsOutputPlatform) Root() outputcap.Directory {
	if platform == nil || platform.native == nil || platform.root == nil || platform.root.native == nil {
		return nil
	}
	return platform.root
}

func (platform *windowsOutputPlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	if platform == nil {
		return ""
	}
	return platform.rootOpenDisposition
}

func (platform *windowsOutputPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return nil, windowsOutputError(err)
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
			windowsOutputError(guard.Close()),
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	return &windowsOutputPublicOperationGuard{
		native: guard,
		root:   &windowsOutputDirectory{native: root},
	}, nil
}

func (guard *windowsOutputPublicOperationGuard) Root() outputcap.Directory {
	if guard == nil || guard.native == nil || guard.root == nil || guard.root.native == nil {
		return nil
	}
	return guard.root
}

func (guard *windowsOutputPublicOperationGuard) Close() error {
	if guard == nil || guard.native == nil {
		return nil
	}
	err := guard.native.Close()
	guard.native = nil
	if guard.root != nil {
		guard.root.native = nil
	}
	guard.root = nil
	return windowsOutputError(err)
}

func (platform *windowsOutputPlatform) IdentityScheme() outputcap.IdentitySchemeID {
	if platform == nil || platform.native == nil || platform.native.Durability() != windowsOutputProcessRestart {
		return ""
	}
	return outputcap.IdentitySchemeWindowsObjectIDV1
}

func (platform *windowsOutputPlatform) RootBinding() (
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
		return outputcap.OutputRootBinding{}, windowsOutputError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, windowsOutputError(guard.Close())) }()
	root := guard.Root()
	if root == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	if _, err := root.prepareIdentityClaim(); err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputError(err)
	}
	facts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputError(
			windowsFailure("bind output root", root.path, errWindowsOutputUnsafe, err),
		)
	}
	if err := windowsValidateOpenedObject(facts, root.volume, true); err != nil {
		return outputcap.OutputRootBinding{}, windowsOutputError(
			windowsFailure("bind output root", root.path, errWindowsOutputUnsafe, err),
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
	if len(guid) == 0 || len(guid) > windowsVolumeGUIDClaimMaxBytes {
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
	binding, err := outputcap.NewOutputRootBinding(platform.IdentityScheme(), volume, object)
	return binding, windowsOutputError(err)
}

func (platform *windowsOutputPlatform) Durability() transfer.DurabilityLevel {
	if platform.IdentityScheme() == "" {
		return transfer.DurabilityNone
	}
	return transfer.DurabilityProcessRestart
}

func (*windowsOutputPlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return checkpointmodel.LiveCleanupWindowsNTFSV1
}

func (platform *windowsOutputPlatform) DestinationCapabilities() (
	_ outputcap.DestinationCapabilities,
	resultErr error,
) {
	if platform == nil || platform.native == nil || platform.native.root == nil {
		return outputcap.DestinationCapabilities{}, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	var guard *windowsPublicOperationGuard
	var err error
	if platform.root.native.private {
		guard, err = platform.native.acquirePrivatePublicationRootGuard()
	} else {
		guard, err = platform.native.acquirePublicOperationGuard()
	}
	if err != nil {
		return outputcap.DestinationCapabilities{}, windowsOutputError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, windowsOutputError(guard.Close())) }()
	capabilities, err := guard.Root().destinationCapabilities()
	if err != nil || platform.IdentityScheme() != "" {
		return capabilities, err
	}
	unsupported, err := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsupportedFilesystem)
	if err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	return outputcap.NewDestinationCapabilities(capabilities.SafePublish(), unsupported, unsupported, unsupported)
}

func (platform *windowsOutputPlatform) ProbeRecoverableFeatures() error {
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

func (*windowsOutputPlatform) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	return windowsOutputError(windowsValidateModifiedTime(modified))
}

func (*windowsOutputPlatform) CanonicalLocatorKey(path string) (string, error) {
	key, err := windowsOutputLocatorKey(path)
	return key, windowsOutputError(err)
}

func (*windowsOutputPlatform) CanonicalComponentKey(name string) (string, error) {
	native, err := windowsRelativePath(name, true)
	if err != nil {
		return "", windowsOutputError(err)
	}
	key, err := windowsNTFSCaseKey(native)
	return key, windowsOutputError(err)
}

func (platform *windowsOutputPlatform) Close() error {
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
	return windowsOutputError(err)
}
