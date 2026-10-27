//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/windshare/windshare/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

type nativeOutputPlatform struct {
	root       *windowsDirectory
	inspector  windowsObjectInspector
	policy     *windowsPrivatePolicy
	durability windowsOutputDurability
}

func openWindowsOutputPlatform(path string) (*nativeOutputPlatform, error) {
	return openWindowsOutputPlatformWithInspectors(path, nativeWindowsObjectInspector{}, nativeWindowsVolumeInspector{})
}

func openWindowsOutputPlatformWithInspectors(
	path string,
	inspector windowsObjectInspector,
	volumes windowsVolumeInspector,
) (*nativeOutputPlatform, error) {
	return openWindowsOutputPlatformWithAuthority(
		path,
		inspector,
		volumes,
		windowsRootDirectoryAccess(),
		windowsDirectoryShareMode(false),
	)
}

func openWindowsPrivateRootParent(path string) (*nativeOutputPlatform, error) {
	return openWindowsOutputPlatformWithAuthority(
		path,
		nativeWindowsObjectInspector{},
		nativeWindowsVolumeInspector{},
		windowsPrivateRootParentAccess(),
		windowsDirectoryShareMode(true),
	)
}

func openWindowsOutputPlatformWithAuthority(
	path string,
	inspector windowsObjectInspector,
	volumes windowsVolumeInspector,
	rootAccess uint32,
	shareMode uint32,
) (*nativeOutputPlatform, error) {
	if inspector == nil || volumes == nil || path == "" {
		return nil, windowsFailure("open output root", path, errWindowsOutputUnsupported, errors.New("missing root or inspector"))
	}
	if rootAccess == 0 {
		return nil, windowsFailure("open output root", path, errWindowsOutputUnsupported, errors.New("missing root access authority"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, windowsFailure("resolve output root", path, errWindowsOutputUnsupported, err)
	}
	handle, _, err := windowsOpenNativeWithOptions(
		0, windowsNTPath(absolute), rootAccess, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE, 0, nil,
		shareMode,
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, windowsFailure(
				"open output root", absolute, errWindowsOutputUnsafe,
				errors.Join(outputfault.ErrAncestryAuthorityDenied, err),
			)
		}
		return nil, windowsNativeOperationFailure("open output root", absolute, err)
	}
	file := os.NewFile(uintptr(handle), absolute)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, windowsFailure("open output root", absolute, errWindowsOutputUnsupported, errors.New("wrap root handle"))
	}
	facts, err := inspector.Inspect(handle)
	if err != nil {
		err = windowsFailure("inspect output root", absolute, errWindowsOutputUnsupported, err)
	} else {
		if shapeErr := validateWindowsRootShape(facts); shapeErr != nil {
			err = windowsFailure("inspect output root", absolute, errWindowsOutputUnsupported, shapeErr)
		}
	}
	var volumeFacts windowsVolumeFacts
	if err == nil {
		// Live mutation needs actual native primitives; restart identity is separate.
		var volumeErr error
		volumeFacts, volumeErr = volumes.InspectVolume(handle)
		if volumeErr != nil {
			err = windowsFailure("inspect output volume", absolute, errWindowsOutputUnsupported, volumeErr)
		} else {
			err = validateWindowsVolumeLiveCapabilities(volumeFacts)
		}
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	policy, err := newWindowsPrivatePolicy()
	if err != nil {
		return nil, errors.Join(windowsFailure("prepare private output ACL", absolute, errWindowsOutputUnsupported, err), file.Close())
	}
	root := &windowsDirectory{
		file: file, metadataHandle: windows.InvalidHandle, path: absolute, volume: facts.object.volume,
		objectIDs: nativeWindowsPersistentObjectIDProvider{}, inspector: inspector, policy: policy,
		objectIDState:     newWindowsPersistentObjectIDState(),
		ancestryAuthority: windowsNativeAncestryAuthorityVerifier{},
		enumerate:         &sync.Mutex{}, placementGuard: true,
	}
	durability := windowsOutputDurability(0)
	if validateWindowsVolumeIdentityScheme(volumeFacts) == nil {
		durability = windowsOutputProcessRestart
	}
	return &nativeOutputPlatform{
		root: root, inspector: inspector, policy: policy, durability: durability,
	}, nil
}

func (platform *nativeOutputPlatform) Durability() windowsOutputDurability {
	if platform == nil {
		return 0
	}
	return platform.durability
}

func (platform *nativeOutputPlatform) Root() *windowsDirectory {
	if platform == nil {
		return nil
	}
	return platform.root
}

func (platform *nativeOutputPlatform) Close() error {
	if platform == nil || platform.root == nil {
		return nil
	}
	err := platform.root.Close()
	platform.root = nil
	return err
}

type windowsCreateObserver any

type windowsDirectory struct {
	file               *os.File
	metadataHandle     windows.Handle
	metadataHandleOpen bool
	metadataOpenErr    error
	path               string
	volume             windowsVolumeIdentity
	objectIDs          windowsPersistentObjectIDProvider
	objectIDState      *windowsPersistentObjectIDState
	inspector          windowsObjectInspector
	policy             *windowsPrivatePolicy
	ancestryAuthority  windowsAncestryAuthorityVerifier
	enumerate          *sync.Mutex
	createObserver     windowsCreateObserver
	private            bool
	placementGuard     bool
	selfPlacementGuard bool
}

func (directory *windowsDirectory) handle() windows.Handle {
	return windows.Handle(directory.file.Fd())
}

func (directory *windowsDirectory) Close() error {
	if directory == nil {
		return nil
	}
	var metadataErr error
	if directory.metadataHandleOpen {
		metadataErr = windows.CloseHandle(directory.metadataHandle)
		directory.metadataHandle = windows.InvalidHandle
		directory.metadataHandleOpen = false
	}
	if directory.file == nil {
		return metadataErr
	}
	err := directory.file.Close()
	directory.file = nil
	return errors.Join(metadataErr, err)
}

func (directory *windowsDirectory) OpenDirectory(relative string) (*windowsDirectory, error) {
	return directory.openDirectory(relative, directory.private, windows.FILE_OPEN)
}

func (directory *windowsDirectory) OpenPrivateDirectory(relative string) (*windowsDirectory, error) {
	return directory.openDirectory(relative, true, windows.FILE_OPEN)
}

func (directory *windowsDirectory) CreatePrivateDirectory(relative string) (*windowsDirectory, error) {
	return directory.createPrivateDirectory(relative)
}

func (directory *windowsDirectory) OpenOrCreatePrivateDirectory(relative string) (*windowsDirectory, bool, error) {
	opened, err := directory.OpenPrivateDirectory(relative)
	if err == nil {
		return opened, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	created, err := directory.CreatePrivateDirectory(relative)
	if !errors.Is(err, errWindowsOutputCollision) {
		return created, err == nil, err
	}
	opened, err = directory.OpenPrivateDirectory(relative)
	return opened, false, err
}

func (directory *windowsDirectory) openDirectory(relative string, private bool, disposition uint32) (*windowsDirectory, error) {
	opened, _, err := directory.openDirectoryStatus(relative, private, disposition)
	return opened, err
}

func (directory *windowsDirectory) openDirectoryStatus(relative string, private bool, disposition uint32) (*windowsDirectory, bool, error) {
	if err := directory.usable(); err != nil {
		return nil, false, err
	}
	if private && disposition != windows.FILE_OPEN {
		return nil, false, windowsFailure(
			"create private output directory", relative, errWindowsOutputUnsafe,
			errors.New("private mutation requires the crash-safe delete-on-close commit protocol"),
		)
	}
	native, err := windowsRelativePath(relative, private)
	if err != nil {
		return nil, false, windowsFailure("open output directory", relative, errWindowsOutputUnsafe, err)
	}
	var descriptor *windows.SECURITY_DESCRIPTOR
	attributes := uint32(0)
	if private {
		descriptor, err = directory.policy.descriptor(true)
		if err != nil {
			return nil, false, windowsFailure("prepare private directory ACL", relative, errWindowsOutputUnsafe, err)
		}
		attributes = windows.FILE_ATTRIBUTE_HIDDEN
	}
	// Public output directories carry locator authority, so their open handles
	// deny delete sharing for the complete operation. A private child disables
	// that guard for its whole subtree because recovery must rename and remove
	// those entries through concurrently retained identity witnesses.
	placementGuard := directory.placementGuard && !private
	handle, status, err := windowsOpenNativeWithOptions(
		directory.handle(), native, windowsOpenedDirectoryAccess(placementGuard), disposition,
		windows.FILE_DIRECTORY_FILE, attributes, descriptor,
		windowsDirectoryShareMode(placementGuard),
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		if disposition == windows.FILE_CREATE {
			return nil, false, windowsNativeNoReplaceFailure("open output directory", relative, err)
		}
		return nil, false, windowsNativeOperationFailure("open output directory", relative, err)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, false, windowsFailure("open output directory", relative, errWindowsOutputUnsafe, errors.New("wrap directory handle"))
	}
	opened := &windowsDirectory{
		file: file, metadataHandle: windows.InvalidHandle,
		path: filepath.Join(directory.path, relative), volume: directory.volume,
		objectIDs: directory.objectIDs, inspector: directory.inspector, policy: directory.policy,
		objectIDState: newWindowsPersistentObjectIDState(), ancestryAuthority: directory.ancestryAuthority,
		enumerate: &sync.Mutex{}, createObserver: directory.createObserver, private: private,
		placementGuard: placementGuard, selfPlacementGuard: placementGuard,
	}
	if err := opened.verify(private); err != nil {
		return nil, false, errors.Join(err, opened.Close())
	}
	if err := windowsVerifyOpenedLeafAuthority(opened.handle(), native, private); err != nil {
		return nil, false, errors.Join(err, opened.Close())
	}
	created, err := windowsCreationStatus(disposition, status)
	if err != nil {
		return nil, false, errors.Join(windowsFailure("classify output directory open", relative, errWindowsOutputUnsafe, err), opened.Close())
	}
	if !private {
		opened.metadataHandle, opened.metadataOpenErr = directory.openDirectoryMetadataHandle(native, opened)
		opened.metadataHandleOpen = opened.metadataOpenErr == nil
	}
	return opened, created, nil
}

func (directory *windowsDirectory) openDirectoryMetadataHandle(
	native string,
	expected *windowsDirectory,
) (windows.Handle, error) {
	handle, _, err := windowsOpenNative(
		directory.handle(), native, windows.FILE_WRITE_ATTRIBUTES|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, 0, nil,
	)
	if err != nil {
		return windows.InvalidHandle, windowsNativeOperationFailure(
			"open output directory metadata authority", expected.path, err,
		)
	}
	expectedIdentity, expectedErr := expected.inspector.Identify(expected.handle())
	metadataFacts, metadataErr := expected.inspector.Inspect(handle)
	same := expectedErr == nil && metadataErr == nil && expectedIdentity.same(metadataFacts.object)
	if !same {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, errors.Join(
			windowsFailure("open output directory metadata authority", expected.path, errWindowsOutputUnsafe,
				errors.New("metadata handle does not identify the retained directory")),
			expectedErr, metadataErr,
		)
	}
	if err := windowsValidateOpenedObject(metadataFacts, expected.volume, true); err != nil {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, windowsFailure(
			"open output directory metadata authority", expected.path, errWindowsOutputUnsafe, err,
		)
	}
	return handle, nil
}

func (directory *windowsDirectory) verify(private bool) error {
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return windowsFailure("inspect output directory", directory.path, errWindowsOutputUnsafe, err)
	}
	if err := windowsValidateOpenedObject(facts, directory.volume, true); err != nil {
		return windowsFailure("inspect output directory", directory.path, errWindowsOutputUnsafe, err)
	}
	if private {
		if facts.attributes&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
			return windowsFailure("verify private output directory", directory.path, errWindowsOutputUnsafe,
				errors.New("directory is not hidden"))
		}
		if err := directory.policy.verify(directory.handle(), true); err != nil {
			return windowsFailure("verify private output directory", directory.path, errWindowsOutputUnsafe, err)
		}
	}
	return nil
}

func (directory *windowsDirectory) usable() error {
	if directory == nil || directory.file == nil || directory.inspector == nil || directory.policy == nil {
		return windowsFailure("use output directory", "", errWindowsOutputUnsafe, errors.New("directory handle is closed or incomplete"))
	}
	return nil
}

func (directory *windowsDirectory) Sync() error {
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
		return windowsNativeOperationFailure("sync output directory", directory.path, err)
	}
	return nil
}
