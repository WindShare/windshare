//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

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
	return openWindowsV3OutputPlatformWithAuthority(
		path,
		inspector,
		windowsV3RootDirectoryAccess(),
		windowsV3DirectoryShareMode(false),
	)
}

func openWindowsV3PrivateRootParent(path string) (*windowsV3OutputPlatform, error) {
	return openWindowsV3OutputPlatformWithAuthority(
		path,
		nativeWindowsV3HandleInspector{},
		windowsV3PrivateRootParentAccess(),
		windowsV3DirectoryShareMode(true),
	)
}

func openWindowsV3OutputPlatformWithAuthority(
	path string,
	inspector windowsV3HandleInspector,
	rootAccess uint32,
	shareMode uint32,
) (*windowsV3OutputPlatform, error) {
	if inspector == nil || path == "" {
		return nil, windowsV3Failure("open output root", path, errWindowsV3OutputUnsupported, errors.New("missing root or inspector"))
	}
	if rootAccess == 0 {
		return nil, windowsV3Failure("open output root", path, errWindowsV3OutputUnsupported, errors.New("missing root access authority"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, windowsV3Failure("resolve output root", path, errWindowsV3OutputUnsupported, err)
	}
	handle, _, err := windowsV3OpenNativeWithOptions(
		0, windowsV3NTPath(absolute), rootAccess, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE, 0, nil,
		shareMode,
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
