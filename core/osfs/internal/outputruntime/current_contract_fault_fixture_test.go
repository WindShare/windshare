package outputruntime

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type currentRuntimeFaultPlan struct {
	injected error

	duplicateErrAt     int
	duplicateCalls     int
	openDirectoryAt    int
	openDirectoryErr   error
	openDirectoryN     int
	createDirectoryAt  int
	createDirectoryErr error
	createDirectoryN   int
	createFileAt       int
	createFileErr      error
	createFileN        int
	openFileAt         int
	openFileErr        error
	openFileN          int
	linkFileAt         int
	linkFileErr        error
	linkFileReturnsNil bool
	linkFileN          int
	removeFileAt       int
	removeFileErr      error
	removeFileN        int
	replaceFileAt      int
	replaceFileErr     error
	replaceFileN       int
	observeAt          int
	observeErr         error
	observeKindAt      int
	observeKind        outputcap.EntryKind
	observeN           int
	classifyAt         int
	classifyErr        error
	classifyKind       outputcap.EntryKind
	classifyExact      bool
	classifyN          int
	directorySyncAt    int
	directorySyncErr   error
	directorySyncN     int
	directoryCloseAt   int
	directoryCloseErr  error
	directoryCloseN    int
	sameDirectoryAt    int
	sameDirectoryErr   error
	sameDirectoryFalse bool
	sameDirectoryN     int
	identityAt         int
	identityErr        error
	identityN          int
	prepareIdentityAt  int
	prepareIdentityErr error
	prepareIdentityN   int

	fileSyncAt        int
	fileSyncErr       error
	fileSyncN         int
	fileSizeAt        int
	fileSizeErr       error
	fileSizeAdjust    int64
	fileSizeN         int
	fileSameAt        int
	fileSameErr       error
	fileSameFalse     bool
	fileSameN         int
	fileMetadataAt    int
	fileMetadataErr   error
	fileMetadataFalse bool
	fileMetadataN     int
	fileModifiedAt    int
	fileModifiedErr   error
	fileModifiedN     int
	fileWriteAt       int
	fileWriteErr      error
	fileWriteN        int
	fileCloseAt       int
	fileCloseErr      error
	fileCloseN        int

	guardErr      error
	guardCloseErr error
}

func (plan *currentRuntimeFaultPlan) failure(specific error) error {
	if specific != nil {
		return specific
	}
	if plan != nil && plan.injected != nil {
		return plan.injected
	}
	return errors.New("injected output-runtime failure")
}

func currentRuntimeFaultHit(counter *int, at int) bool {
	*counter += 1
	return at > 0 && *counter == at
}

type currentRuntimeFaultDirectory struct {
	outputcap.Directory
	plan *currentRuntimeFaultPlan
}

func currentWrapFaultDirectory(
	directory outputcap.Directory,
	plan *currentRuntimeFaultPlan,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &currentRuntimeFaultDirectory{Directory: directory, plan: plan}
}

func currentUnwrapFaultDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*currentRuntimeFaultDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func (directory *currentRuntimeFaultDirectory) Duplicate() (outputcap.Directory, error) {
	if currentRuntimeFaultHit(&directory.plan.duplicateCalls, directory.plan.duplicateErrAt) {
		return nil, directory.plan.failure(nil)
	}
	duplicate, err := directory.Directory.Duplicate()
	return currentWrapFaultDirectory(duplicate, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if currentRuntimeFaultHit(&directory.plan.openDirectoryN, directory.plan.openDirectoryAt) {
		return nil, directory.plan.failure(directory.plan.openDirectoryErr)
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	return currentWrapFaultDirectory(opened, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if currentRuntimeFaultHit(&directory.plan.createDirectoryN, directory.plan.createDirectoryAt) {
		return nil, directory.plan.failure(directory.plan.createDirectoryErr)
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	return currentWrapFaultDirectory(created, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if currentRuntimeFaultHit(&directory.plan.createFileN, directory.plan.createFileAt) {
		return nil, directory.plan.failure(directory.plan.createFileErr)
	}
	created, err := directory.Directory.CreateFile(name, private, size)
	return currentWrapFaultFile(created, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if currentRuntimeFaultHit(&directory.plan.openFileN, directory.plan.openFileAt) {
		return nil, directory.plan.failure(directory.plan.openFileErr)
	}
	opened, err := directory.Directory.OpenFile(name, private, writable)
	return currentWrapFaultFile(opened, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) LinkFileNoReplace(
	source outputcap.File,
	name string,
) (outputcap.File, error) {
	if currentRuntimeFaultHit(&directory.plan.linkFileN, directory.plan.linkFileAt) {
		return nil, directory.plan.failure(directory.plan.linkFileErr)
	}
	if directory.plan.linkFileReturnsNil {
		return nil, nil
	}
	linked, err := directory.Directory.LinkFileNoReplace(currentUnwrapFaultFile(source), name)
	return currentWrapFaultFile(linked, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	if currentRuntimeFaultHit(&directory.plan.replaceFileN, directory.plan.replaceFileAt) {
		return directory.plan.failure(directory.plan.replaceFileErr)
	}
	return directory.Directory.ReplacePrivateFile(currentUnwrapFaultFile(source), name)
}

func (directory *currentRuntimeFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if currentRuntimeFaultHit(&directory.plan.removeFileN, directory.plan.removeFileAt) {
		return directory.plan.failure(directory.plan.removeFileErr)
	}
	return directory.Directory.RemoveFile(name, currentUnwrapFaultFile(expected))
}

func (directory *currentRuntimeFaultDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(currentUnwrapFaultDirectory(candidate), name)
	return currentWrapFaultDirectory(installed, directory.plan), err
}

func (directory *currentRuntimeFaultDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(name, currentUnwrapFaultDirectory(expected))
}

func (directory *currentRuntimeFaultDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	directory.plan.observeN++
	if directory.plan.observeAt > 0 && directory.plan.observeN == directory.plan.observeAt {
		return outputcap.EntryAbsent, directory.plan.failure(directory.plan.observeErr)
	}
	if directory.plan.observeKindAt > 0 && directory.plan.observeN == directory.plan.observeKindAt {
		return directory.plan.observeKind, nil
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *currentRuntimeFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if currentRuntimeFaultHit(&directory.plan.classifyN, directory.plan.classifyAt) {
		if directory.plan.classifyErr != nil {
			return outputcap.EntryAbsent, false, directory.plan.classifyErr
		}
		return directory.plan.classifyKind, directory.plan.classifyExact, nil
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *currentRuntimeFaultDirectory) Sync() error {
	if currentRuntimeFaultHit(&directory.plan.directorySyncN, directory.plan.directorySyncAt) {
		return directory.plan.failure(directory.plan.directorySyncErr)
	}
	return directory.Directory.Sync()
}

func (directory *currentRuntimeFaultDirectory) Close() error {
	directory.plan.directoryCloseN++
	closeErr := directory.Directory.Close()
	if directory.plan.directoryCloseAt > 0 && directory.plan.directoryCloseN == directory.plan.directoryCloseAt {
		return errors.Join(closeErr, directory.plan.failure(directory.plan.directoryCloseErr))
	}
	return closeErr
}

func (directory *currentRuntimeFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if currentRuntimeFaultHit(&directory.plan.sameDirectoryN, directory.plan.sameDirectoryAt) {
		if directory.plan.sameDirectoryErr != nil {
			return false, directory.plan.sameDirectoryErr
		}
		if directory.plan.sameDirectoryFalse {
			return false, nil
		}
		return false, directory.plan.failure(nil)
	}
	return directory.Directory.SameDirectory(currentUnwrapFaultDirectory(other))
}

func (directory *currentRuntimeFaultDirectory) IdentityClaim() (outputcap.PersistentDirectoryIdentity, error) {
	if currentRuntimeFaultHit(&directory.plan.identityN, directory.plan.identityAt) {
		return outputcap.PersistentDirectoryIdentity{}, directory.plan.failure(directory.plan.identityErr)
	}
	return directory.Directory.IdentityClaim()
}

func (directory *currentRuntimeFaultDirectory) PrepareIdentityClaim() (outputcap.PersistentDirectoryIdentity, error) {
	if currentRuntimeFaultHit(&directory.plan.prepareIdentityN, directory.plan.prepareIdentityAt) {
		return outputcap.PersistentDirectoryIdentity{}, directory.plan.failure(directory.plan.prepareIdentityErr)
	}
	return directory.Directory.PrepareIdentityClaim()
}

type currentRuntimeFaultFile struct {
	outputcap.File
	plan *currentRuntimeFaultPlan
}

func currentWrapFaultFile(file outputcap.File, plan *currentRuntimeFaultPlan) outputcap.File {
	if file == nil {
		return nil
	}
	return &currentRuntimeFaultFile{File: file, plan: plan}
}

func currentUnwrapFaultFile(file outputcap.File) outputcap.File {
	if wrapped, ok := file.(*currentRuntimeFaultFile); ok {
		return wrapped.File
	}
	return file
}

func (file *currentRuntimeFaultFile) Sync() error {
	if currentRuntimeFaultHit(&file.plan.fileSyncN, file.plan.fileSyncAt) {
		return file.plan.failure(file.plan.fileSyncErr)
	}
	return file.File.Sync()
}

func (file *currentRuntimeFaultFile) Size() (uint64, error) {
	file.plan.fileSizeN++
	if file.plan.fileSizeAt > 0 && file.plan.fileSizeN == file.plan.fileSizeAt {
		if file.plan.fileSizeErr != nil {
			return 0, file.plan.fileSizeErr
		}
		size, err := file.File.Size()
		if err != nil || file.plan.fileSizeAdjust == 0 {
			return size, err
		}
		if file.plan.fileSizeAdjust > 0 {
			return size + uint64(file.plan.fileSizeAdjust), nil
		}
		return size - uint64(-file.plan.fileSizeAdjust), nil
	}
	return file.File.Size()
}

func (file *currentRuntimeFaultFile) SameFile(other outputcap.File) (bool, error) {
	if currentRuntimeFaultHit(&file.plan.fileSameN, file.plan.fileSameAt) {
		if file.plan.fileSameErr != nil {
			return false, file.plan.fileSameErr
		}
		if file.plan.fileSameFalse {
			return false, nil
		}
		return false, file.plan.failure(nil)
	}
	return file.File.SameFile(currentUnwrapFaultFile(other))
}

func (file *currentRuntimeFaultFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if currentRuntimeFaultHit(&file.plan.fileMetadataN, file.plan.fileMetadataAt) {
		if file.plan.fileMetadataErr != nil {
			return false, file.plan.fileMetadataErr
		}
		if file.plan.fileMetadataFalse {
			return false, nil
		}
		return false, file.plan.failure(nil)
	}
	return file.File.MetadataMatches(size, modified)
}

func (file *currentRuntimeFaultFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if currentRuntimeFaultHit(&file.plan.fileModifiedN, file.plan.fileModifiedAt) {
		return file.plan.failure(file.plan.fileModifiedErr)
	}
	return file.File.SetModifiedTime(modified)
}

func (file *currentRuntimeFaultFile) WriteAt(data []byte, offset int64) (int, error) {
	if currentRuntimeFaultHit(&file.plan.fileWriteN, file.plan.fileWriteAt) {
		return 0, file.plan.failure(file.plan.fileWriteErr)
	}
	return file.File.WriteAt(data, offset)
}

func (file *currentRuntimeFaultFile) Close() error {
	file.plan.fileCloseN++
	closeErr := file.File.Close()
	if file.plan.fileCloseAt > 0 && file.plan.fileCloseN == file.plan.fileCloseAt {
		return errors.Join(closeErr, file.plan.failure(file.plan.fileCloseErr))
	}
	return closeErr
}

type currentRuntimeFaultPlatform struct {
	outputcap.Platform
	plan *currentRuntimeFaultPlan
}

func (platform *currentRuntimeFaultPlatform) Root() outputcap.Directory {
	return currentWrapFaultDirectory(platform.Platform.Root(), platform.plan)
}

func (platform *currentRuntimeFaultPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform.plan.guardErr != nil {
		return nil, platform.plan.guardErr
	}
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	return &currentRuntimeFaultGuard{
		PublicOperationGuard: guard,
		root:                 currentWrapFaultDirectory(guard.Root(), platform.plan),
		closeErr:             platform.plan.guardCloseErr,
	}, nil
}

type currentRuntimeFaultGuard struct {
	outputcap.PublicOperationGuard
	root     outputcap.Directory
	closeErr error
}

func (guard *currentRuntimeFaultGuard) Root() outputcap.Directory { return guard.root }

func (guard *currentRuntimeFaultGuard) Close() error {
	if guard == nil {
		return nil
	}
	err := errors.Join(guard.PublicOperationGuard.Close(), guard.closeErr)
	guard.PublicOperationGuard = nil
	guard.root = nil
	return err
}
