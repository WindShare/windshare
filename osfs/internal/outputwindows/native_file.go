//go:build windows

package outputwindows

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows"
)

type windowsFile struct {
	file      *os.File
	path      string
	volume    windowsVolumeIdentity
	inspector windowsObjectInspector
	policy    *windowsPrivatePolicy
}

func (file *windowsFile) handle() windows.Handle { return windows.Handle(file.file.Fd()) }

func (file *windowsFile) Close() error {
	if file == nil || file.file == nil {
		return nil
	}
	err := file.file.Close()
	file.file = nil
	return err
}

func (file *windowsFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.file == nil {
		return 0, windowsFailure("read output file", "", errWindowsOutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.ReadAt(destination, offset)
}

func (file *windowsFile) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.file == nil {
		return 0, windowsFailure("write output file", "", errWindowsOutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.WriteAt(source, offset)
}

func (file *windowsFile) Truncate(size int64) error {
	if file == nil || file.file == nil {
		return windowsFailure("size output file", "", errWindowsOutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.Truncate(size)
}

func (file *windowsFile) Sync() error {
	if file == nil || file.file == nil {
		return windowsFailure("sync output file", "", errWindowsOutputUnsafe, errors.New("file handle is closed"))
	}
	return file.file.Sync()
}

func (directory *windowsDirectory) CreatePrivateFile(relative string) (*windowsFile, error) {
	opened, _, err := directory.openPrivateFile(relative, windows.FILE_CREATE)
	return opened, err
}

func (directory *windowsDirectory) OpenPrivateFile(relative string) (*windowsFile, error) {
	opened, _, err := directory.openPrivateFile(relative, windows.FILE_OPEN)
	return opened, err
}

func (directory *windowsDirectory) openOrCreatePrivateFile(relative string) (*windowsFile, bool, error) {
	return directory.openPrivateFile(relative, windows.FILE_OPEN_IF)
}

func (directory *windowsDirectory) openPrivateFile(relative string, disposition uint32) (*windowsFile, bool, error) {
	descriptor, err := directory.policy.descriptor(false)
	if err != nil {
		return nil, false, windowsFailure("prepare private file ACL", relative, errWindowsOutputUnsafe, err)
	}
	return directory.openFile(relative, disposition, windowsPrivateFileAccess(), descriptor, true)
}

const (
	windowsLiveStageTemporaryPrefix      = ".windshare-live-stage-"
	windowsLiveStageTemporaryRandomBytes = 16
	windowsLiveStageAllocationAttempts   = 16
)

type windowsLiveStageCreateCut uint8

const (
	windowsLiveStageCutTemporaryCreated windowsLiveStageCreateCut = iota + 1
	windowsLiveStageCutInstalled
	windowsLiveStageCutSynced
	windowsLiveStageCutCommitted
)

type windowsLiveStageCreateObserver interface {
	ObserveLiveStageCreate(windowsLiveStageCreateCut) error
}

type windowsLiveStageCreateObserverFunc func(windowsLiveStageCreateCut) error

func (observe windowsLiveStageCreateObserverFunc) ObserveLiveStageCreate(cut windowsLiveStageCreateCut) error {
	return observe(cut)
}

func (directory *windowsDirectory) observeLiveStageCreate(cut windowsLiveStageCreateCut) error {
	if directory.createObserver == nil {
		return nil
	}
	observer, ok := directory.createObserver.(windowsLiveStageCreateObserver)
	if !ok {
		return nil
	}
	return observer.ObserveLiveStageCreate(cut)
}

func (directory *windowsDirectory) createPublicInheritedDeleteOnCloseFile() (*windowsFile, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if directory.private {
		return nil, windowsFailure("create live-cleanup stage", directory.path, errWindowsOutputUnsafe,
			errors.New("stage allocation requires the exact public final parent"))
	}
	for range windowsLiveStageAllocationAttempts {
		var nonce [windowsLiveStageTemporaryRandomBytes]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, windowsFailure("allocate live-cleanup stage name", directory.path,
				errWindowsOutputUnsafe, err)
		}
		name := windowsLiveStageTemporaryPrefix + hex.EncodeToString(nonce[:])
		native, err := windowsRelativePath(name, true)
		if err != nil {
			return nil, windowsFailure("allocate live-cleanup stage name", name, errWindowsOutputUnsafe, err)
		}
		handle, _, err := windowsOpenNative(
			directory.handle(), native, windowsPrivateFileAccess(), windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_DELETE_ON_CLOSE, windows.FILE_ATTRIBUTE_NORMAL, nil,
		)
		if errors.Is(err, fs.ErrExist) || windowsIsCollision(err) {
			continue
		}
		if err != nil {
			return nil, windowsNativeOperationFailure("create live-cleanup stage", name, err)
		}
		wrapped := os.NewFile(uintptr(handle), name)
		if wrapped == nil {
			_ = windows.CloseHandle(handle)
			return nil, windowsFailure("create live-cleanup stage", name, errWindowsOutputUnsafe,
				errors.New("wrap stage file handle"))
		}
		stage := &windowsFile{
			file: wrapped, path: filepath.Join(directory.path, name), volume: directory.volume,
			inspector: directory.inspector, policy: directory.policy,
		}
		if err := errors.Join(stage.verify(false), windowsVerifyOpenedLeafAuthority(stage.handle(), native, false)); err != nil {
			return nil, errors.Join(err, stage.Close())
		}
		if err := directory.observeLiveStageCreate(windowsLiveStageCutTemporaryCreated); err != nil {
			return nil, errors.Join(err, stage.Close())
		}
		return stage, nil
	}
	return nil, windowsFailure("create live-cleanup stage", directory.path, errWindowsOutputUnsafe,
		errors.New("could not allocate a unique temporary stage name"))
}

func (directory *windowsDirectory) moveLiveStageNoReplace(
	stage *windowsFile,
	proofDirectory *windowsDirectory,
	target string,
) error {
	if err := directory.usable(); err != nil {
		return err
	}
	if err := proofDirectory.usable(); err != nil {
		return err
	}
	if stage == nil || stage.file == nil || directory.private || !proofDirectory.private ||
		stage.volume != directory.volume || proofDirectory.volume != directory.volume {
		return windowsFailure("install live-cleanup stage", target, errWindowsOutputUnsafe,
			errors.New("stage and proof-directory authorities are incompatible"))
	}
	name, err := windowsRelativePath(target, true)
	if err != nil {
		return windowsFailure("install live-cleanup stage", target, errWindowsOutputUnsafe, err)
	}
	buffer, err := windowsLinkRenameBuffer(windows.FILE_RENAME_POSIX_SEMANTICS, proofDirectory.handle(), name)
	if err != nil {
		return windowsFailure("install live-cleanup stage", target, errWindowsOutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsNTError(windows.NtSetInformationFile(
		stage.handle(), &status, &buffer[0], uint32(len(buffer)), windowsFileRenameInformationEx,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(proofDirectory)
	runtime.KeepAlive(stage)
	if err != nil {
		return windowsNativeNoReplaceFailure("install live-cleanup stage", target, err)
	}
	if err := directory.observeLiveStageCreate(windowsLiveStageCutInstalled); err != nil {
		return err
	}
	if err := errors.Join(stage.Sync(), directory.Sync(), proofDirectory.Sync()); err != nil {
		return windowsFailure("sync installed live-cleanup stage", target, errWindowsOutputUnsafe, err)
	}
	if err := directory.observeLiveStageCreate(windowsLiveStageCutSynced); err != nil {
		return err
	}
	installed, _, err := proofDirectory.openFile(
		target, windows.FILE_OPEN, windowsReadFileAccess(), nil, false,
	)
	if err != nil {
		return windowsFailure("verify installed live-cleanup stage", target, errWindowsOutputUnsafe, err)
	}
	same, compareErr := sameWindowsOpenedObject(stage, installed)
	closeErr := installed.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(windowsFailure("verify installed live-cleanup stage", target, errWindowsOutputUnsafe,
			errors.New("proof-directory entry does not identify the staged object")), compareErr, closeErr)
	}
	if err := windowsCommitDeleteOnClose(stage.handle()); err != nil {
		return windowsFailure("commit installed live-cleanup stage", target, errWindowsOutputUnsafe, err)
	}
	if err := directory.observeLiveStageCreate(windowsLiveStageCutCommitted); err != nil {
		return err
	}
	return proofDirectory.Sync()
}

func (directory *windowsDirectory) OpenRegularFile(relative string) (*windowsFile, error) {
	opened, _, err := directory.openFile(
		relative, windows.FILE_OPEN, windowsReadFileAccess(), nil, directory.private,
	)
	return opened, err
}

func (directory *windowsDirectory) openFileForDelete(relative string, private bool) (*windowsFile, error) {
	opened, _, err := directory.openFile(
		relative, windows.FILE_OPEN, windowsDeleteFileAccess(), nil, private,
	)
	return opened, err
}

func (directory *windowsDirectory) openFile(
	relative string,
	disposition uint32,
	access uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
	private bool,
) (*windowsFile, bool, error) {
	if disposition == windows.FILE_CREATE {
		file, err := directory.createFile(relative, access, descriptor, private, 0)
		return file, err == nil, err
	}
	if err := directory.usable(); err != nil {
		return nil, false, err
	}
	native, err := windowsRelativePath(relative, private)
	if err != nil {
		return nil, false, windowsFailure("open output file", relative, errWindowsOutputUnsafe, err)
	}
	handle, status, err := windowsOpenNative(
		directory.handle(), native, access, disposition, windows.FILE_NON_DIRECTORY_FILE,
		windows.FILE_ATTRIBUTE_NORMAL, descriptor,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, false, windowsNativeOperationFailure("open output file", relative, err)
	}
	wrapped := os.NewFile(uintptr(handle), relative)
	if wrapped == nil {
		_ = windows.CloseHandle(handle)
		return nil, false, windowsFailure("open output file", relative, errWindowsOutputUnsafe, errors.New("wrap file handle"))
	}
	file := &windowsFile{
		file: wrapped, path: filepath.Join(directory.path, relative), volume: directory.volume,
		inspector: directory.inspector, policy: directory.policy,
	}
	if err := file.verify(private); err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	if err := windowsVerifyOpenedLeafAuthority(file.handle(), native, private); err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	created, err := windowsCreationStatus(disposition, status)
	if err != nil {
		return nil, false, errors.Join(windowsFailure("classify output file open", relative, errWindowsOutputUnsafe, err), file.Close())
	}
	return file, created, nil
}

func (file *windowsFile) verify(private bool) error {
	if file == nil || file.file == nil || file.inspector == nil {
		return windowsFailure("inspect output file", "", errWindowsOutputUnsafe, errors.New("file handle is closed or incomplete"))
	}
	facts, err := file.inspector.Inspect(file.handle())
	if err != nil {
		return windowsFailure("inspect output file", file.path, errWindowsOutputUnsafe, err)
	}
	if err := windowsValidateOpenedObject(facts, file.volume, false); err != nil {
		return windowsFailure("inspect output file", file.path, errWindowsOutputUnsafe, err)
	}
	if private {
		if err := file.policy.verify(file.handle(), false); err != nil {
			return windowsFailure("verify private output file", file.path, errWindowsOutputUnsafe, err)
		}
	}
	return nil
}

func windowsValidateOpenedObject(facts windowsObjectFacts, expected windowsVolumeIdentity, directory bool) error {
	// Volume capabilities were certified on admission. Current GUID and serial
	// membership is checked even for handles opened relative to that authority.
	if facts.object.volume != expected {
		return errors.New("opened object crossed the certified NTFS volume boundary")
	}
	if facts.attributes&windowsCloudAttributeMask != 0 {
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

func sameWindowsOpenedObject(left, right *windowsFile) (bool, error) {
	if left == nil || right == nil || left.file == nil || right.file == nil || left.inspector == nil || right.inspector == nil {
		return false, windowsFailure("compare output objects", "", errWindowsOutputUnsafe, errors.New("missing open file handle"))
	}
	leftFacts, leftErr := left.inspector.Identify(left.handle())
	rightFacts, rightErr := right.inspector.Identify(right.handle())
	if leftErr != nil || rightErr != nil {
		return false, windowsFailure("compare output objects", "", errWindowsOutputUnsafe, errors.Join(leftErr, rightErr))
	}
	return leftFacts.same(rightFacts), nil
}

func sameWindowsOpenedDirectory(left, right *windowsDirectory) (bool, error) {
	if left == nil || right == nil || left.file == nil || right.file == nil || left.inspector == nil || right.inspector == nil {
		return false, windowsFailure("compare output directories", "", errWindowsOutputUnsafe, errors.New("missing open directory handle"))
	}
	leftFacts, leftErr := left.inspector.Identify(left.handle())
	rightFacts, rightErr := right.inspector.Identify(right.handle())
	if leftErr != nil || rightErr != nil {
		return false, windowsFailure("compare output directories", "", errWindowsOutputUnsafe, errors.Join(leftErr, rightErr))
	}
	return leftFacts.same(rightFacts), nil
}

type windowsPublishMutationError struct {
	cause error
}

func (failure *windowsPublishMutationError) Error() string {
	return fmt.Sprintf("native publication may be visible: %v", failure.cause)
}

func (failure *windowsPublishMutationError) Unwrap() error { return failure.cause }

func windowsPublicationMayBeVisible(err error) bool {
	_, ok := errors.AsType[*windowsPublishMutationError](err)
	return ok
}

func (directory *windowsDirectory) LinkRegularFileNoReplace(source *windowsFile, target string) (*windowsFile, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if source == nil || source.file == nil || source.volume != directory.volume {
		return nil, windowsFailure("link output file", target, errWindowsOutputUnsafe, errors.New("source is absent or on another volume"))
	}
	name, err := windowsRelativePath(target, true)
	if err != nil {
		return nil, windowsFailure("link output file", target, errWindowsOutputUnsafe, err)
	}
	buffer, err := windowsLinkRenameBuffer(0, directory.handle(), name)
	if err != nil {
		return nil, windowsFailure("link output file", target, errWindowsOutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsNTError(windows.NtSetInformationFile(
		source.handle(), &status, &buffer[0], uint32(len(buffer)), windows.FileLinkInformation,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(source)
	if err != nil {
		return nil, windowsNativeNoReplaceFailure("link output file", target, err)
	}
	if err := directory.Sync(); err != nil {
		return nil, &windowsPublishMutationError{cause: err}
	}
	// A protected namespace may hold an ordinary-profile stage or anchor whose
	// access profile must survive same-object publication. The source capability
	// already established its profile; reopening the new name only needs native
	// identity and no-follow validation because a hard link cannot change its DACL.
	linked, _, err := directory.openFile(
		target, windows.FILE_OPEN, windowsReadFileAccess(), nil, false,
	)
	if err != nil {
		return nil, &windowsPublishMutationError{cause: windowsFailure(
			"verify linked output file", target, errWindowsOutputUnsafe, err)}
	}
	same, err := sameWindowsOpenedObject(source, linked)
	if err != nil || !same {
		return nil, &windowsPublishMutationError{cause: errors.Join(
			windowsFailure("verify linked output file", target, errWindowsOutputUnsafe,
				errors.New("destination does not identify the source object")),
			err, linked.Close())}
	}
	return linked, nil
}

func (directory *windowsDirectory) AtomicReplacePrivateFile(source *windowsFile, target string) error {
	if err := directory.usable(); err != nil {
		return err
	}
	if source == nil || source.file == nil || source.volume != directory.volume {
		return windowsFailure("replace private state", target, errWindowsOutputUnsafe, errors.New("source is absent or on another volume"))
	}
	name, err := windowsRelativePath(target, true)
	if err != nil {
		return windowsFailure("replace private state", target, errWindowsOutputUnsafe, err)
	}
	flags := uint32(windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS |
		windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE)
	buffer, err := windowsLinkRenameBuffer(flags, directory.handle(), name)
	if err != nil {
		return windowsFailure("replace private state", target, errWindowsOutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsNTError(windows.NtSetInformationFile(
		source.handle(), &status, &buffer[0], uint32(len(buffer)), windowsFileRenameInformationEx,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(source)
	if err != nil {
		return windowsNativeOperationFailure("replace private state", target, err)
	}
	installed, err := directory.OpenPrivateFile(target)
	if err != nil {
		return windowsFailure("verify replaced private state", target, errWindowsOutputUnsafe, err)
	}
	same, compareErr := sameWindowsOpenedObject(source, installed)
	closeErr := installed.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(windowsFailure("verify replaced private state", target, errWindowsOutputUnsafe,
			errors.New("installed state does not identify the source object")), compareErr, closeErr)
	}
	return nil
}

func (directory *windowsDirectory) InstallPrivateDirectoryNoReplace(source *windowsDirectory, target string) (*windowsDirectory, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if source == nil || source.file == nil || source.volume != directory.volume {
		return nil, windowsFailure("install private directory", target, errWindowsOutputUnsafe,
			errors.New("source is absent or on another volume"))
	}
	name, err := windowsRelativePath(target, true)
	if err != nil {
		return nil, windowsFailure("install private directory", target, errWindowsOutputUnsafe, err)
	}
	buffer, err := windowsLinkRenameBuffer(windows.FILE_RENAME_POSIX_SEMANTICS, directory.handle(), name)
	if err != nil {
		return nil, windowsFailure("install private directory", target, errWindowsOutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsNTError(windows.NtSetInformationFile(
		source.handle(), &status, &buffer[0], uint32(len(buffer)), windowsFileRenameInformationEx,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(source)
	if err != nil {
		switch {
		case windowsIsCollision(err):
			return nil, windowsNativeNoReplaceFailure("install private directory", target, err)
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
			return nil, windowsDirectoryInstallDeniedFailure(target, err, openErr, closeErr)
		default:
			return nil, windowsNativeNoReplaceFailure("install private directory", target, err)
		}
	}
	if err := directory.Sync(); err != nil {
		return nil, &windowsPublishMutationError{cause: err}
	}
	installed, err := directory.OpenPrivateDirectory(target)
	if err != nil {
		return nil, &windowsPublishMutationError{cause: windowsFailure(
			"verify installed private directory", target, errWindowsOutputUnsafe, err)}
	}
	same, compareErr := sameWindowsOpenedDirectory(source, installed)
	if compareErr != nil || !same {
		return nil, &windowsPublishMutationError{cause: errors.Join(
			windowsFailure("verify installed private directory", target, errWindowsOutputUnsafe,
				errors.New("installed directory does not identify the fixed candidate")),
			compareErr, installed.Close())}
	}
	return installed, nil
}

func windowsDirectoryInstallDeniedFailure(target string, installErr, observationErr, closeErr error) error {
	if closeErr != nil {
		// A close failure leaves the handle lifecycle unsettled. Treating the
		// preceding observation as a clean collision would let bootstrap skip
		// the failure solely because it recognizes the collision category.
		return windowsOperationalFailure(
			"close private directory collision observation", target,
			errors.Join(installErr, observationErr, closeErr),
		)
	}
	if observationErr == nil {
		return windowsFailure("install private directory", target, errWindowsOutputCollision, installErr)
	}
	if errors.Is(observationErr, errWindowsOutputUnsafe) {
		return windowsFailure(
			"install private directory", target, errWindowsOutputUnsafe,
			errors.Join(installErr, observationErr),
		)
	}
	return windowsNativeOperationFailure(
		"install private directory", target, errors.Join(installErr, observationErr),
	)
}
