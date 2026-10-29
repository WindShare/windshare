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

type directoryOrigin struct {
	parent *directoryHandle
	name   string
}

type directory struct {
	*directoryHandle
	origin *directoryOrigin
}

func validComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func newDirectory(fd int, volume volumeEvidence, parent *directoryHandle, name string, private bool) (*directory, error) {
	handle, err := newDirectoryHandle(fd, volume, name, private)
	if err != nil {
		return nil, err
	}
	dir := &directory{directoryHandle: handle}
	if parent != nil {
		copy, err := parent.duplicate()
		if err != nil {
			return nil, errors.Join(err, dir.Close())
		}
		dir.origin = &directoryOrigin{parent: copy, name: name}
	}
	return dir, nil
}

func (dir *directory) verify() error {
	if dir == nil {
		return os.ErrClosed
	}
	return dir.directoryHandle.verify()
}

func (dir *directory) Close() error {
	if dir == nil {
		return nil
	}
	err := dir.directoryHandle.Close()
	if dir.origin != nil {
		err = errors.Join(err, dir.origin.parent.Close())
		dir.origin = nil
	}
	return err
}

func (dir *directory) Duplicate() (outputcap.Directory, error) {
	if dir == nil {
		return nil, os.ErrClosed
	}
	handle, err := dir.directoryHandle.duplicate()
	if err != nil {
		return nil, err
	}
	copy := &directory{directoryHandle: handle}
	if dir.origin != nil {
		parent, err := dir.origin.parent.duplicate()
		if err != nil {
			return nil, errors.Join(err, copy.Close())
		}
		copy.origin = &directoryOrigin{parent: parent, name: dir.origin.name}
	}
	return copy, nil
}
func (dir *directory) Sync() error {
	if err := dir.verify(); err != nil {
		return err
	}
	return dir.file.Sync()
}
func (dir *directory) Names(limit int) ([]string, error) {
	batch, err := dir.ReadNameBatch(limit)
	if batch.HasMore {
		err = errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	return batch.Names, err
}

func (dir *directory) ReadNameBatch(limit int) (outputcap.DirectoryNameBatch, error) {
	if limit < 0 {
		return outputcap.DirectoryNameBatch{}, outputcap.ErrUnsafeNamespace
	}
	copy, err := dir.Duplicate()
	if err != nil {
		return outputcap.DirectoryNameBatch{}, err
	}
	names, readErr := copy.(*directory).file.Readdirnames(limit + 1)
	closeErr := copy.Close()
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	return outputcap.DirectoryNameBatch{
		Names: names[:min(len(names), limit)], HasMore: len(names) > limit,
	}, errors.Join(readErr, closeErr)
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
	if dir == nil {
		return nil, os.ErrClosed
	}
	child, err := dir.directoryHandle.openDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return child, nil
}
func (dir *directory) createDirectoryEntry(name string, mode uint32) error {
	if err := errors.Join(dir.ValidateCreateAuthority(), validComponent(name)); err != nil {
		return err
	}
	return nativeError(unix.Mkdirat(dir.fd(), name, mode))
}

func (dir *directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	mode := uint32(publicDirectoryMode)
	if private {
		mode = privateDirectoryMode
	}
	if err := dir.createDirectoryEntry(name, mode); err != nil {
		return nil, err
	}
	created, err := dir.OpenDirectory(name, false)
	if err != nil {
		return nil, err
	}
	child := created.(*directory)
	if private {
		if err := child.establishPrivateAuthority(); err != nil {
			child.private = false
			return nil, errors.Join(err, dir.RemoveDirectory(name, child), child.Close())
		}
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
	// APFS advertises non-recycled persistent file IDs. Creation time is writable
	// metadata on Darwin, so it cannot participate in an object identity claim.
	const domain = "darwin/apfs/directory-object/v1"
	claim := append([]byte(domain), dir.volume.uuid[:]...)
	claim = binary.BigEndian.AppendUint64(claim, dir.object.inode)
	return claim, nil
}
func (dir *directory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	return dir.PersistentDirectoryIdentityClaim()
}

var _ outputcap.Directory = (*directory)(nil)
