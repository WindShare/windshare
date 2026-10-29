//go:build darwin

package outputdarwin

import (
	"errors"
	"os"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

type objectIdentity struct {
	device int32
	inode  uint64
	kind   uint16
}

func identity(stat unix.Stat_t) objectIdentity {
	return objectIdentity{device: stat.Dev, inode: stat.Ino, kind: stat.Mode & unix.S_IFMT}
}

// A retained native handle owns one object, never its ancestry. Origins retain
// this smaller capability so file and directory copies have bounded fd costs.
type directoryHandle struct {
	file    *os.File
	object  objectIdentity
	volume  volumeEvidence
	private bool
}

func newDirectoryHandle(fd int, volume volumeEvidence, name string, private bool) (*directoryHandle, error) {
	handle := &directoryHandle{file: os.NewFile(uintptr(fd), name), volume: volume, private: private}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(nativeError(err), handle.Close())
	}
	handle.object = identity(stat)
	if handle.object.kind != unix.S_IFDIR || handle.object.device != volume.device {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, handle.Close())
	}
	if err := handle.verify(); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	return handle, nil
}

func (handle *directoryHandle) fd() int { return int(handle.file.Fd()) }

func (handle *directoryHandle) verify() error {
	if handle == nil || handle.file == nil {
		return os.ErrClosed
	}
	var stat unix.Stat_t
	if err := unix.Fstat(handle.fd(), &stat); err != nil {
		return nativeError(err)
	}
	if identity(stat) != handle.object || stat.Nlink == 0 {
		return outputcap.ErrUnsafeNamespace
	}
	if handle.private {
		if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
			return outputcap.ErrUnsafeNamespace
		}
		return validatePrivateACL(handle.fd())
	}
	return nil
}

func (handle *directoryHandle) Close() error {
	if handle == nil || handle.file == nil {
		return nil
	}
	err := handle.file.Close()
	handle.file = nil
	return err
}

func (handle *directoryHandle) duplicate() (*directoryHandle, error) {
	if err := handle.verify(); err != nil {
		return nil, err
	}
	// A fresh open description keeps directory enumeration cursors independent.
	fd, err := unix.Openat(handle.fd(), ".", directoryOpenFlags, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	copy, err := newDirectoryHandle(fd, handle.volume, handle.file.Name(), handle.private)
	if err != nil {
		return nil, err
	}
	if copy.object != handle.object {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, copy.Close())
	}
	return copy, nil
}

func (handle *directoryHandle) openDirectory(name string, private bool) (*directory, error) {
	if err := errors.Join(handle.verify(), validComponent(name)); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(handle.fd(), name, directoryOpenFlags, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	return newDirectory(fd, handle.volume, handle, name, private)
}
