//go:build linux

package osfs

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

type linuxV3Platform struct {
	root *linuxV3Directory
}

type linuxV3DirectoryOrigin struct {
	parent *linuxOutputDirectory
	name   string
}

type linuxV3Directory struct {
	native         *linuxOutputDirectory
	origin         *linuxV3DirectoryOrigin
	metadataPolicy *linuxSelectionMetadataPolicy
	selectionPath  string
}

type linuxV3FileOrigin struct {
	parent *linuxOutputDirectory
	name   string
}

type linuxV3File struct {
	native   *linuxOutputRegularFile
	origin   *linuxV3FileOrigin
	private  bool
	borrowed bool
}

type linuxV3Lock struct {
	native *linuxOutputStableLock
	file   *linuxV3File
}

type linuxV3EntryRef struct {
	native *linuxOutputPinnedEntry
}

func openOutputV3Platform(path string, create bool) (outputV3Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(errOutputV3Unsafe,
			linuxUnsafe("open output root", "output root must be absolute", nil))
	}
	clean := filepath.Clean(path)
	root, err := linuxOpenExt4OutputRoot(clean, &linuxHostOutputSystem)
	if create && errors.Is(err, fs.ErrNotExist) {
		root, err = linuxCreateCertifiedOutputRoot(clean)
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3Platform{root: &linuxV3Directory{native: root}}, nil
}

func linuxCreateCertifiedOutputRoot(path string) (_ *linuxOutputDirectory, resultErr error) {
	const operation = "create certified output root"
	candidate := path
	missing := make([]string, 0, 4)
	var current *linuxOutputDirectory
	for {
		opened, err := linuxOpenExt4OutputRoot(candidate, &linuxHostOutputSystem)
		if err == nil {
			current = opened
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil, errors.Join(linuxUnsafe(operation,
				"no existing certified ancestor contains the requested root", nil), err)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
	defer func() {
		if current != nil {
			resultErr = errors.Join(resultErr, current.close())
		}
	}()
	for index := len(missing) - 1; index >= 0; index-- {
		created, err := current.createDirectoryExact(missing[index], uint32(dirPerm))
		if err != nil {
			return nil, err
		}
		if err := current.close(); err != nil {
			return nil, errors.Join(err, created.close())
		}
		current = created
	}

	// Reopening the full path proves that the requested spelling still resolves
	// to the handle-created object; a concurrent rename cannot redirect the new
	// root between safe creation and authority return.
	reopened, err := linuxOpenExt4OutputRoot(path, &linuxHostOutputSystem)
	if err != nil {
		return nil, err
	}
	same, compareErr := linuxSameOpenDirectory(current, reopened)
	if compareErr != nil || !same {
		return nil, errors.Join(linuxUnsafe(operation,
			"reopened root differs from the handle-created directory", nil), compareErr, reopened.close())
	}
	if err := current.close(); err != nil {
		return nil, errors.Join(err, reopened.close())
	}
	current = nil
	return reopened, nil
}

func (platform *linuxV3Platform) Root() outputV3Directory {
	if platform == nil {
		return nil
	}
	return platform.root
}

func (platform *linuxV3Platform) AcquirePublicOperationGuard() (outputV3PublicOperationGuard, error) {
	root := platform.Root()
	if root == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux output platform is closed"))
	}
	// Linux's handle-relative ancestry walk proves placement for every operation;
	// the guard makes that proof an explicit platform capability while borrowing
	// the already pinned certified root.
	return &borrowedOutputPublicOperationGuard{root: root}, nil
}

func (*linuxV3Platform) Certification() resumestate.CertificationID {
	return resumestate.CertificationLinuxExt4ProcessRestart
}

func (platform *linuxV3Platform) RootBinding() (resumestate.OutputRootBinding, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return resumestate.OutputRootBinding{}, errors.Join(
			errOutputV3Unsafe,
			errors.New("osfs: Linux output platform is closed"),
		)
	}
	root := platform.root.native
	if err := root.verifyHandle(); err != nil {
		return resumestate.OutputRootBinding{}, linuxV3Error(err)
	}
	certificate := root.certificate
	volume := make([]byte, len("linux/ext4/volume/v1")+8+4+4+4+4)
	copy(volume, "linux/ext4/volume/v1")
	offset := len("linux/ext4/volume/v1")
	binary.BigEndian.PutUint64(volume[offset:], certificate.mount.uniqueMountID)
	offset += 8
	binary.BigEndian.PutUint32(volume[offset:], certificate.mount.deviceMajor)
	offset += 4
	binary.BigEndian.PutUint32(volume[offset:], certificate.mount.deviceMinor)
	offset += 4
	binary.BigEndian.PutUint32(volume[offset:], uint32(certificate.mount.filesystemID[0]))
	offset += 4
	binary.BigEndian.PutUint32(volume[offset:], uint32(certificate.mount.filesystemID[1]))

	if !certificate.rootObject.hasGeneration || certificate.rootObject.generation == 0 {
		return resumestate.OutputRootBinding{}, errors.Join(
			errOutputV3Unsupported,
			errors.New("osfs: Linux output root has no non-reused ext4 incarnation"),
		)
	}
	object := make([]byte, len("linux/ext4/directory-object/v2")+8+4+2)
	copy(object, "linux/ext4/directory-object/v2")
	offset = len("linux/ext4/directory-object/v2")
	binary.BigEndian.PutUint64(object[offset:], certificate.rootObject.inode)
	offset += 8
	binary.BigEndian.PutUint32(object[offset:], certificate.rootObject.generation)
	offset += 4
	binary.BigEndian.PutUint16(object[offset:], linuxFileType(certificate.rootObject.mode))
	binding, err := resumestate.NewOutputRootBinding(platform.Certification(), volume, object)
	return binding, linuxV3Error(err)
}

func (*linuxV3Platform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}

func (platform *linuxV3Platform) ProbeRecoverableFeatures() error {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux output platform is closed"))
	}
	return linuxV3Error(platform.root.native.probeRecoverableFeatures())
}

func (*linuxV3Platform) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	return linuxV3Error(linuxValidateModifiedTime(modified))
}

func (*linuxV3Platform) CanonicalLocatorKey(path string) (string, error) {
	key, err := linuxOutputLocatorKey(path)
	return key, linuxV3Error(err)
}

func (*linuxV3Platform) CanonicalComponentKey(name string) (string, error) {
	if err := linuxValidateComponent("canonicalize output component", name); err != nil {
		return "", linuxV3Error(err)
	}
	return name, nil
}

func (platform *linuxV3Platform) Close() error {
	if platform == nil || platform.root == nil {
		return nil
	}
	err := platform.root.Close()
	platform.root = nil
	return linuxV3Error(err)
}

func (directory *linuxV3Directory) Close() error {
	if directory == nil {
		return nil
	}
	var originErr error
	if directory.origin != nil && directory.origin.parent != nil {
		originErr = directory.origin.parent.close()
	}
	directory.origin = nil
	var nativeErr error
	if directory.native != nil {
		nativeErr = directory.native.close()
	}
	directory.native = nil
	return linuxV3Error(errors.Join(originErr, nativeErr))
}

func (directory *linuxV3Directory) Duplicate() (outputV3Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(err)
	}
	result := &linuxV3Directory{
		native: native, metadataPolicy: directory.metadataPolicy, selectionPath: directory.selectionPath,
	}
	if directory.origin != nil {
		parent, duplicateErr := directory.origin.parent.Duplicate()
		if duplicateErr != nil {
			return nil, linuxV3Error(errors.Join(duplicateErr, native.close()))
		}
		result.origin = &linuxV3DirectoryOrigin{parent: parent, name: directory.origin.name}
	}
	return result, nil
}

func (directory *linuxV3Directory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.sync())
}

func (directory *linuxV3Directory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, linuxV3Error(err)
}

func (directory *linuxV3Directory) NamesWithPrefix(prefix string, matchLimit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	names, err := directory.native.namesWithPrefix(prefix, matchLimit)
	return names, linuxV3Error(err)
}

func (directory *linuxV3Directory) ObserveEntry(name string) (outputV3EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputV3EntryAbsent, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, linuxV3Error(err)
}

func (directory *linuxV3Directory) ClassifyExactEntry(name string) (outputV3EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputV3EntryAbsent, false, errors.Join(
			errOutputV3Unsafe,
			errors.New("osfs: Linux directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, linuxV3Error(err)
}

func (directory *linuxV3Directory) ValidatePublicEntryName(name string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	_, exact, err := directory.native.classifyExactEntry(name)
	if err != nil {
		return linuxV3Error(err)
	}
	if !exact {
		return errOutputV3Unsafe
	}
	return nil
}

func (directory *linuxV3Directory) ValidatePublicEntryNames(names []string) error {
	for _, name := range names {
		if err := directory.ValidatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

func (directory *linuxV3Directory) ValidateCreateAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.validateCreateAuthority())
}

func (directory *linuxV3Directory) ValidateMetadataAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	if err := directory.native.validateMetadataAuthority(); err != nil {
		return linuxV3Error(err)
	}
	if directory.metadataPolicy != nil &&
		directory.metadataPolicy.requiresExtendedTimestamp(directory.selectionPath) {
		return linuxV3Error(linuxRequireExtendedTimestampLayout(
			directory.native.system,
			directory.native.fd,
			directory.native.certificate,
			unix.S_IFDIR,
			"validate selected directory timestamp layout",
		))
	}
	return nil
}

func (directory *linuxV3Directory) OpenEntry(name string) (outputV3EntryRef, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3EntryRef{native: pinned}, nil
}

func (directory *linuxV3Directory) EntryMatches(name string, expected outputV3EntryRef) (bool, error) {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, linuxV3Error(err)
}

func (directory *linuxV3Directory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3Directory{native: opened}, nil
}

func (directory *linuxV3Directory) RemoveEntry(name string, expected outputV3EntryRef) error {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux pinned entry removal"))
	}
	return linuxV3Error(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *linuxV3Directory) PrepareIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	claim, err := directory.native.prepareIdentityClaim()
	return claim, linuxV3Error(err)
}

func (directory *linuxV3Directory) IdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	claim, err := directory.native.identityClaim()
	return claim, linuxV3Error(err)
}

func (directory *linuxV3Directory) SameDirectory(other outputV3Directory) (bool, error) {
	right, ok := other.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux directory authority"))
	}
	same, err := linuxSameOpenDirectory(directory.native, right.native)
	return same, linuxV3Error(err)
}

func (directory *linuxV3Directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.setModifiedTime(modified))
}

func (directory *linuxV3Directory) OpenDirectory(name string, private bool) (outputV3Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	var opened *linuxOutputDirectory
	var err error
	if private {
		opened, err = directory.native.openDirectoryExact(name, linuxOutputDirectoryMode)
	} else {
		opened, err = directory.native.openDirectory(name)
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindDirectoryOrigin(opened, name)
}

func (directory *linuxV3Directory) CreateDirectory(name string, private bool) (outputV3Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	var created *linuxOutputDirectory
	var err error
	if private {
		created, err = directory.native.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
	} else {
		created, err = directory.native.createDirectoryExact(name, uint32(dirPerm))
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindDirectoryOrigin(created, name)
}

func (directory *linuxV3Directory) bindDirectoryOrigin(
	child *linuxOutputDirectory,
	name string,
) (outputV3Directory, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, child.close()))
	}
	return &linuxV3Directory{
		native:         child,
		origin:         &linuxV3DirectoryOrigin{parent: parent, name: name},
		metadataPolicy: directory.metadataPolicy,
		selectionPath:  linuxSelectionChildPath(directory.selectionPath, name),
	}, nil
}

func linuxSelectionChildPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func (directory *linuxV3Directory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	source, ok := candidate.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil ||
		source.origin == nil || source.origin.parent == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux installation candidate has no fixed origin"))
	}
	if err := source.origin.parent.renameDirectory(
		source.origin.name, source.native, directory.native, name, linuxRenameNoReplace,
	); err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.OpenDirectory(name, true)
}

func (directory *linuxV3Directory) RemoveDirectory(name string, expected outputV3Directory) error {
	target, ok := expected.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux directory removal authority"))
	}
	return linuxV3Error(directory.native.unlinkDirectory(name, target.native))
}

func (directory *linuxV3Directory) CreateFile(name string, private bool, size int64) (outputV3File, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	permissions := uint32(filePerm)
	if private {
		permissions = linuxOutputStateFileMode
	}
	created, err := directory.native.createRegularFileExact(name, permissions, size)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, created.close()))
	}
	return &linuxV3File{
		native: created, private: private,
		origin: &linuxV3FileOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) OpenFile(name string, private, writable bool) (outputV3File, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	var opened *linuxOutputRegularFile
	var err error
	if private {
		opened, err = directory.native.openRegularFileExact(name, writable, linuxOutputStateFileMode)
	} else {
		opened, err = directory.native.openRegularFile(name, writable)
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindFileOrigin(opened, name, private)
}

func (directory *linuxV3Directory) LinkFileNoReplace(source outputV3File, name string) (outputV3File, error) {
	file, ok := source.(*linuxV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil ||
		file.origin == nil || file.origin.parent == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux file link authority"))
	}
	if err := directory.native.linkRegularFileNoReplace(
		file.origin.parent, file.origin.name, file.native, name,
	); err != nil {
		return nil, linuxV3Error(err)
	}
	linked, err := directory.native.openRegularFile(name, false)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindFileOrigin(linked, name, false)
}

func (directory *linuxV3Directory) bindFileOrigin(
	file *linuxOutputRegularFile,
	name string,
	private bool,
) (outputV3File, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, file.close()))
	}
	return &linuxV3File{
		native: file, private: private,
		origin: &linuxV3FileOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) ReplacePrivateFile(source outputV3File, name string) error {
	file, ok := source.(*linuxV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil ||
		!file.private || file.origin == nil || file.origin.parent == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux state replacement source has no private fixed origin"))
	}
	return linuxV3Error(file.origin.parent.renameRegularFile(
		file.origin.name, file.native, directory.native, name, linuxRenameReplace,
	))
}

func (directory *linuxV3Directory) RemoveFile(name string, expected outputV3File) error {
	file, ok := expected.(*linuxV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux file removal authority"))
	}
	return linuxV3Error(directory.native.unlinkRegularFile(name, file.native))
}

func (directory *linuxV3Directory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.acquireExistingStableLock(name)
		if err != nil {
			return nil, false, linuxV3Error(err)
		}
		return newLinuxV3Lock(lock), false, nil
	}
	file, err := directory.native.createRegularFileExact(name, linuxOutputStateFileMode, 0)
	created := err == nil
	if errors.Is(err, errLinuxOutputCollision) {
		lock, existingErr := directory.native.acquireExistingStableLock(name)
		if existingErr != nil {
			return nil, false, linuxV3Error(existingErr)
		}
		return newLinuxV3Lock(lock), false, nil
	}
	if err != nil {
		return nil, false, linuxV3Error(err)
	}
	lock, err := linuxLockStableFile(file)
	if err != nil {
		return nil, false, linuxV3Error(errors.Join(err, file.close()))
	}
	matches, err := directory.native.regularEntryMatches(name, file)
	if err != nil || !matches {
		return nil, false, linuxV3Error(errors.Join(
			linuxUnsafe("lock stable output authority", "lock name differs from the locked object", nil),
			err,
			lock.Close(),
		))
	}
	return newLinuxV3Lock(lock), created, nil
}

func (entry *linuxV3EntryRef) Kind() outputV3EntryKind {
	if entry == nil || entry.native == nil {
		return outputV3EntryAbsent
	}
	return entry.native.kind
}

func (entry *linuxV3EntryRef) AllocatedSize() (uint64, error) {
	if entry == nil || entry.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux pinned entry is closed"))
	}
	size, err := entry.native.allocatedSize()
	return size, linuxV3Error(err)
}

func (entry *linuxV3EntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return linuxV3Error(err)
}

func newLinuxV3Lock(lock *linuxOutputStableLock) *linuxV3Lock {
	file := &linuxV3File{private: true, borrowed: true}
	if lock != nil {
		file.native = lock.file
	}
	return &linuxV3Lock{native: lock, file: file}
}

func (file *linuxV3File) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	count, err := file.native.ReadAt(destination, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3File) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	count, err := file.native.WriteAt(source, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3File) Close() error {
	if file == nil {
		return nil
	}
	if file.borrowed {
		return nil
	}
	var originErr error
	if file.origin != nil && file.origin.parent != nil {
		originErr = file.origin.parent.close()
	}
	file.origin = nil
	var nativeErr error
	if file.native != nil {
		nativeErr = file.native.close()
	}
	file.native = nil
	return linuxV3Error(errors.Join(originErr, nativeErr))
}

func (file *linuxV3File) Sync() error {
	if file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	return linuxV3Error(file.native.sync())
}

func (file *linuxV3File) Truncate(size int64) error {
	if file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	return linuxV3Error(file.native.truncate(size))
}

func (file *linuxV3File) Size() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	size, err := file.native.Size()
	return size, linuxV3Error(err)
}

func (file *linuxV3File) AllocatedSize() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	size, err := file.native.allocatedSize()
	return size, linuxV3Error(err)
}

func (file *linuxV3File) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	return linuxV3Error(file.native.setModifiedTime(modified))
}

func (file *linuxV3File) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: Linux file authority is closed"))
	}
	matches, err := file.native.metadataMatches(size, modified)
	return matches, linuxV3Error(err)
}

func (file *linuxV3File) SameFile(other outputV3File) (bool, error) {
	right, ok := other.(*linuxV3File)
	if !ok || file == nil || file.native == nil || right == nil || right.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Linux file authority"))
	}
	same, err := linuxSameOpenRegularFile(file.native, right.native)
	return same, linuxV3Error(err)
}

func (lock *linuxV3Lock) File() outputV3File {
	if lock == nil {
		return nil
	}
	return lock.file
}

func (lock *linuxV3Lock) Close() error {
	if lock == nil || lock.native == nil {
		return nil
	}
	err := lock.native.Close()
	lock.native = nil
	if lock.file != nil {
		lock.file.native = nil
	}
	return linuxV3Error(err)
}

func linuxV3Error(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errLinuxOutputUnsupported):
		return errors.Join(errOutputV3Unsupported, err)
	case errors.Is(err, errLinuxOutputUnsafe):
		return errors.Join(errOutputV3Unsafe, err)
	case errors.Is(err, errLinuxOutputCollision):
		return errors.Join(errOutputV3Collision, err)
	case errors.Is(err, errLinuxOutputLockBusy):
		return errors.Join(errOutputV3LockBusy, err)
	case errors.Is(err, fs.ErrExist):
		return errors.Join(errOutputV3Collision, err)
	default:
		return err
	}
}

var (
	_ outputV3Platform  = (*linuxV3Platform)(nil)
	_ outputV3Directory = (*linuxV3Directory)(nil)
	_ outputV3File      = (*linuxV3File)(nil)
	_ outputV3Lock      = (*linuxV3Lock)(nil)
)
