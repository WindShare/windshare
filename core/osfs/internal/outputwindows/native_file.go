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
	"strings"

	"golang.org/x/sys/windows"
)

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

const (
	windowsV3LiveStageTemporaryPrefix      = ".windshare-live-stage-"
	windowsV3LiveStageTemporaryRandomBytes = 16
	windowsV3LiveStageAllocationAttempts   = 16
)

type windowsV3LiveStageCreateCut uint8

const (
	windowsV3LiveStageCutTemporaryCreated windowsV3LiveStageCreateCut = iota + 1
	windowsV3LiveStageCutInstalled
	windowsV3LiveStageCutSynced
	windowsV3LiveStageCutCommitted
)

type windowsV3LiveStageCreateObserver interface {
	ObserveLiveStageCreate(windowsV3LiveStageCreateCut) error
}

type windowsV3LiveStageCreateObserverFunc func(windowsV3LiveStageCreateCut) error

func (observe windowsV3LiveStageCreateObserverFunc) ObserveLiveStageCreate(cut windowsV3LiveStageCreateCut) error {
	return observe(cut)
}

func (directory *windowsV3Directory) observeLiveStageCreate(cut windowsV3LiveStageCreateCut) error {
	if directory.createObserver == nil {
		return nil
	}
	observer, ok := directory.createObserver.(windowsV3LiveStageCreateObserver)
	if !ok {
		return nil
	}
	return observer.ObserveLiveStageCreate(cut)
}

func (directory *windowsV3Directory) createPublicInheritedDeleteOnCloseFile() (*windowsV3File, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if directory.private {
		return nil, windowsV3Failure("create live-cleanup stage", directory.path, errWindowsV3OutputUnsafe,
			errors.New("stage allocation requires the exact public final parent"))
	}
	for range windowsV3LiveStageAllocationAttempts {
		var nonce [windowsV3LiveStageTemporaryRandomBytes]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, windowsV3Failure("allocate live-cleanup stage name", directory.path,
				errWindowsV3OutputUnsafe, err)
		}
		name := windowsV3LiveStageTemporaryPrefix + hex.EncodeToString(nonce[:])
		native, err := windowsV3RelativePath(name, true)
		if err != nil {
			return nil, windowsV3Failure("allocate live-cleanup stage name", name, errWindowsV3OutputUnsafe, err)
		}
		handle, _, err := windowsV3OpenNative(
			directory.handle(), native, windowsV3PrivateFileAccess(), windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_DELETE_ON_CLOSE, windows.FILE_ATTRIBUTE_NORMAL, nil,
		)
		if errors.Is(err, fs.ErrExist) || windowsV3IsCollision(err) {
			continue
		}
		if err != nil {
			return nil, windowsV3NativeOperationFailure("create live-cleanup stage", name, err)
		}
		wrapped := os.NewFile(uintptr(handle), name)
		if wrapped == nil {
			_ = windows.CloseHandle(handle)
			return nil, windowsV3Failure("create live-cleanup stage", name, errWindowsV3OutputUnsafe,
				errors.New("wrap stage file handle"))
		}
		stage := &windowsV3File{
			file: wrapped, path: filepath.Join(directory.path, name), volume: directory.volume,
			inspector: directory.inspector, policy: directory.policy,
		}
		if err := errors.Join(stage.verify(false), windowsV3VerifyOpenedLeafAuthority(stage.handle(), native, false)); err != nil {
			return nil, errors.Join(err, stage.Close())
		}
		if err := directory.observeLiveStageCreate(windowsV3LiveStageCutTemporaryCreated); err != nil {
			return nil, errors.Join(err, stage.Close())
		}
		return stage, nil
	}
	return nil, windowsV3Failure("create live-cleanup stage", directory.path, errWindowsV3OutputUnsafe,
		errors.New("could not allocate a unique temporary stage name"))
}

func (directory *windowsV3Directory) moveLiveStageNoReplace(
	stage *windowsV3File,
	proofDirectory *windowsV3Directory,
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
		return windowsV3Failure("install live-cleanup stage", target, errWindowsV3OutputUnsafe,
			errors.New("stage and proof-directory authorities are incompatible"))
	}
	name, err := windowsV3RelativePath(target, true)
	if err != nil {
		return windowsV3Failure("install live-cleanup stage", target, errWindowsV3OutputUnsafe, err)
	}
	buffer, err := windowsV3LinkRenameBuffer(windows.FILE_RENAME_POSIX_SEMANTICS, proofDirectory.handle(), name)
	if err != nil {
		return windowsV3Failure("install live-cleanup stage", target, errWindowsV3OutputUnsafe, err)
	}
	var status windows.IO_STATUS_BLOCK
	err = normalizeWindowsV3NTError(windows.NtSetInformationFile(
		stage.handle(), &status, &buffer[0], uint32(len(buffer)), windowsV3FileRenameInformationEx,
	))
	runtime.KeepAlive(directory)
	runtime.KeepAlive(proofDirectory)
	runtime.KeepAlive(stage)
	if err != nil {
		return windowsV3NativeNoReplaceFailure("install live-cleanup stage", target, err)
	}
	if err := directory.observeLiveStageCreate(windowsV3LiveStageCutInstalled); err != nil {
		return err
	}
	if err := errors.Join(stage.Sync(), directory.Sync(), proofDirectory.Sync()); err != nil {
		return windowsV3Failure("sync installed live-cleanup stage", target, errWindowsV3OutputUnsafe, err)
	}
	if err := directory.observeLiveStageCreate(windowsV3LiveStageCutSynced); err != nil {
		return err
	}
	installed, _, err := proofDirectory.openFile(
		target, windows.FILE_OPEN, windowsV3ReadFileAccess(), nil, false,
	)
	if err != nil {
		return windowsV3Failure("verify installed live-cleanup stage", target, errWindowsV3OutputUnsafe, err)
	}
	same, compareErr := sameWindowsV3OpenedObject(stage, installed)
	closeErr := installed.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(windowsV3Failure("verify installed live-cleanup stage", target, errWindowsV3OutputUnsafe,
			errors.New("proof-directory entry does not identify the staged object")), compareErr, closeErr)
	}
	if err := windowsV3CommitDeleteOnClose(stage.handle()); err != nil {
		return windowsV3Failure("commit installed live-cleanup stage", target, errWindowsV3OutputUnsafe, err)
	}
	if err := directory.observeLiveStageCreate(windowsV3LiveStageCutCommitted); err != nil {
		return err
	}
	return proofDirectory.Sync()
}

func (directory *windowsV3Directory) OpenRegularFile(relative string) (*windowsV3File, error) {
	opened, _, err := directory.openFile(
		relative, windows.FILE_OPEN, windowsV3ReadFileAccess(), nil, directory.private,
	)
	return opened, err
}

func (directory *windowsV3Directory) openFileForDelete(relative string, private bool) (*windowsV3File, error) {
	opened, _, err := directory.openFile(
		relative, windows.FILE_OPEN, windowsV3DeleteFileAccess(), nil, private,
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

type windowsV3PublishMutationError struct {
	cause error
}

func (failure *windowsV3PublishMutationError) Error() string {
	return fmt.Sprintf("native publication may be visible: %v", failure.cause)
}

func (failure *windowsV3PublishMutationError) Unwrap() error { return failure.cause }

func windowsV3PublicationMayBeVisible(err error) bool {
	_, ok := errors.AsType[*windowsV3PublishMutationError](err)
	return ok
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
	if err := directory.Sync(); err != nil {
		return nil, &windowsV3PublishMutationError{cause: err}
	}
	// A protected namespace may hold an ordinary-profile stage or anchor whose
	// access profile must survive same-object publication. The source capability
	// already established its profile; reopening the new name only needs native
	// identity and no-follow validation because a hard link cannot change its DACL.
	linked, _, err := directory.openFile(
		target, windows.FILE_OPEN, windowsV3ReadFileAccess(), nil, false,
	)
	if err != nil {
		return nil, &windowsV3PublishMutationError{cause: windowsV3Failure(
			"verify linked output file", target, errWindowsV3OutputUnsafe, err)}
	}
	same, err := sameWindowsV3OpenedObject(source, linked)
	if err != nil || !same {
		return nil, &windowsV3PublishMutationError{cause: errors.Join(
			windowsV3Failure("verify linked output file", target, errWindowsV3OutputUnsafe,
				errors.New("destination does not identify the source object")),
			err, linked.Close())}
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
	if err := directory.Sync(); err != nil {
		return nil, &windowsV3PublishMutationError{cause: err}
	}
	installed, err := directory.OpenPrivateDirectory(target)
	if err != nil {
		return nil, &windowsV3PublishMutationError{cause: windowsV3Failure(
			"verify installed private directory", target, errWindowsV3OutputUnsafe, err)}
	}
	same, compareErr := sameWindowsV3OpenedDirectory(source, installed)
	if compareErr != nil || !same {
		return nil, &windowsV3PublishMutationError{cause: errors.Join(
			windowsV3Failure("verify installed private directory", target, errWindowsV3OutputUnsafe,
				errors.New("installed directory does not identify the fixed candidate")),
			compareErr, installed.Close())}
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
