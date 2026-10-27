//go:build darwin

package outputdarwin

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

type objectIdentity struct {
	device int32
	inode  uint64
	kind   uint16
	birth  unix.Timespec
}

func identity(stat unix.Stat_t) objectIdentity {
	return objectIdentity{device: stat.Dev, inode: stat.Ino, kind: stat.Mode & unix.S_IFMT, birth: stat.Btim}
}

type directory struct {
	file    *os.File
	object  objectIdentity
	volume  volumeEvidence
	parent  *directory
	name    string
	private bool
}

func validComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func newDirectory(fd int, volume volumeEvidence, parent *directory, name string, private bool) (*directory, error) {
	dir := &directory{file: os.NewFile(uintptr(fd), name), volume: volume, name: name, private: private}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(nativeError(err), dir.Close())
	}
	dir.object = identity(stat)
	if dir.object.kind != unix.S_IFDIR || dir.object.device != volume.device {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, dir.Close())
	}
	if parent != nil {
		copy, err := parent.Duplicate()
		if err != nil {
			return nil, errors.Join(err, dir.Close())
		}
		dir.parent = copy.(*directory)
	}
	if err := dir.verify(); err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	return dir, nil
}

func (dir *directory) fd() int { return int(dir.file.Fd()) }
func (dir *directory) verify() error {
	if dir == nil || dir.file == nil {
		return os.ErrClosed
	}
	var stat unix.Stat_t
	if err := unix.Fstat(dir.fd(), &stat); err != nil {
		return nativeError(err)
	}
	if identity(stat) != dir.object || stat.Nlink == 0 {
		return outputcap.ErrUnsafeNamespace
	}
	if dir.private {
		if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
			return outputcap.ErrUnsafeNamespace
		}
		return validateACL(dir.fd(), true)
	}
	return nil
}
func (dir *directory) ValidateCreateAuthority() error {
	if err := dir.verify(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(dir.fd(), &stat); err != nil {
		return nativeError(err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 || stat.Mode&0o300 != 0o300 {
		return outputcap.ErrUnsafeNamespace
	}
	return validateACL(dir.fd(), dir.private)
}
func (dir *directory) ValidateMetadataAuthority() error { return dir.ValidateCreateAuthority() }
func (dir *directory) Close() error {
	if dir == nil {
		return nil
	}
	var err error
	if dir.file != nil {
		err = dir.file.Close()
		dir.file = nil
	}
	if dir.parent != nil {
		err = errors.Join(err, dir.parent.Close())
		dir.parent = nil
	}
	return err
}
func (dir *directory) Duplicate() (outputcap.Directory, error) {
	if err := dir.verify(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dir.fd(), ".", directoryOpenFlags, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	return newDirectory(fd, dir.volume, dir.parent, dir.name, dir.private)
}
func (dir *directory) Sync() error {
	if err := dir.verify(); err != nil {
		return err
	}
	return dir.file.Sync()
}
func (dir *directory) Names(limit int) ([]string, error) {
	if limit < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	copy, err := dir.Duplicate()
	if err != nil {
		return nil, err
	}
	names, readErr := copy.(*directory).file.Readdirnames(limit + 1)
	closeErr := copy.Close()
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if len(names) > limit {
		readErr = errors.Join(readErr, outputcap.ErrUnsafeNamespace)
	}
	return names, errors.Join(readErr, closeErr)
}
func entryKind(mode uint16) outputcap.EntryKind {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return outputcap.EntryRegularFile
	case unix.S_IFDIR:
		return outputcap.EntryDirectory
	default:
		return outputcap.EntryOther
	}
}
func (dir *directory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if err := errors.Join(dir.verify(), validComponent(name)); err != nil {
		return 0, err
	}
	var stat unix.Stat_t
	err := unix.Fstatat(dir.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, os.ErrNotExist) {
		return outputcap.EntryAbsent, nil
	}
	if err != nil {
		return 0, nativeError(err)
	}
	return entryKind(stat.Mode), nil
}
func (dir *directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	kind, err := dir.ObserveEntry(name)
	if err != nil || kind == outputcap.EntryAbsent {
		return kind, err == nil, err
	}
	ref, err := dir.OpenEntry(name)
	if err != nil {
		return kind, false, err
	}
	entry := ref.(*entryReference)
	attrs, readErr := attributes(int(entry.file.Fd()), unix.ATTR_CMN_NAME, 0)
	closeErr := ref.Close()
	if readErr != nil || len(attrs) < 8 {
		return kind, false, errors.Join(readErr, closeErr, outputcap.ErrUnsafeNamespace)
	}
	offset := int(int32(binary.LittleEndian.Uint32(attrs)))
	length := int(binary.LittleEndian.Uint32(attrs[4:]))
	if offset < 8 || length < 1 || offset > len(attrs)-length {
		return kind, false, outputcap.ErrUnsafeNamespace
	}
	actual := unix.ByteSliceToString(attrs[offset : offset+length])
	return kind, norm.NFC.String(actual) == norm.NFC.String(name), closeErr
}
func (dir *directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if err := errors.Join(dir.verify(), validComponent(name)); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dir.fd(), name, directoryOpenFlags, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	return newDirectory(fd, dir.volume, dir, name, private)
}
func (dir *directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if err := errors.Join(dir.ValidateCreateAuthority(), validComponent(name)); err != nil {
		return nil, err
	}
	mode := uint32(publicDirectoryMode)
	if private {
		mode = privateDirectoryMode
	}
	if err := unix.Mkdirat(dir.fd(), name, mode); err != nil {
		return nil, nativeError(err)
	}
	created, err := dir.OpenDirectory(name, false)
	if err != nil {
		return nil, err
	}
	child := created.(*directory)
	child.private = private
	if err := child.verify(); err != nil {
		child.private = false
		return nil, errors.Join(err, dir.RemoveDirectory(name, child), child.Close())
	}
	return child, nil
}
func (dir *directory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*directory)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.verify(), right.verify()); err != nil {
		return false, err
	}
	return dir.object == right.object && dir.volume.filesystem == right.volume.filesystem, nil
}
func (dir *directory) SetModifiedTime(value catalog.ModifiedTime) error {
	if err := dir.ValidateMetadataAuthority(); err != nil {
		return err
	}
	return setModifiedTime(dir.fd(), value)
}

func (dir *directory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	if err := dir.verify(); err != nil {
		return nil, err
	}
	if !dir.volume.restart {
		return nil, outputcap.ErrRecoverableOutputUnsupported
	}
	fileID, err := attributes(dir.fd(), unix.ATTR_CMN_FILEID, 0)
	if err != nil {
		return nil, err
	}
	if len(fileID) != 8 || binary.LittleEndian.Uint64(fileID) != dir.object.inode {
		return nil, outputcap.ErrUnsafeNamespace
	}
	// Birth time is additive replacement evidence. Content-generation counters
	// are deliberately absent: ordinary writes must never change object identity.
	const domain = "darwin/apfs/directory-object/v1"
	claim := append([]byte(domain), dir.volume.uuid[:]...)
	claim = binary.BigEndian.AppendUint64(claim, dir.object.inode)
	claim = binary.BigEndian.AppendUint64(claim, uint64(dir.object.birth.Sec))
	claim = binary.BigEndian.AppendUint64(claim, uint64(dir.object.birth.Nsec))
	return claim, nil
}
func (dir *directory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	return dir.PersistentDirectoryIdentityClaim()
}

var _ outputcap.Directory = (*directory)(nil)
