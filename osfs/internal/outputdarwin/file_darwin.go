//go:build darwin

package outputdarwin

import (
	"errors"
	"os"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputfile"
	"golang.org/x/sys/unix"
)

type fileState struct {
	file     *os.File
	parent   *directory
	name     string
	object   objectIdentity
	private  bool
	borrowed bool
}
type observedFile = outputfile.Observed[*fileState]
type mutableFile = outputfile.Mutable[*fileState]
type recoveryFile = outputfile.Recovery[*fileState]

func stateFrom(file outputcap.FileIdentity) (*fileState, bool) {
	switch value := file.(type) {
	case *observedFile:
		if value != nil {
			return value.State, value.State != nil
		}
	case *mutableFile:
		if value != nil {
			return value.State, value.State != nil
		}
	case *recoveryFile:
		if value != nil {
			return value.State, value.State != nil
		}
	}
	return nil, false
}
func (state *fileState) verify() error {
	if state == nil || state.file == nil {
		return outputfile.ErrClosed
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(state.file.Fd()), &stat); err != nil {
		return nativeError(err)
	}
	if identity(stat) != state.object || state.object.kind != unix.S_IFREG {
		return outputcap.ErrUnsafeNamespace
	}
	if state.private {
		if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
			return outputcap.ErrUnsafeNamespace
		}
		return validateACL(int(state.file.Fd()), true)
	}
	return nil
}
func (state *fileState) Close() error {
	if state == nil || state.borrowed {
		return nil
	}
	var err error
	if state.file != nil {
		err = state.file.Close()
		state.file = nil
	}
	if state.parent != nil {
		err = errors.Join(err, state.parent.Close())
		state.parent = nil
	}
	return err
}
func (state *fileState) Size() (uint64, error) {
	if err := state.verify(); err != nil {
		return 0, err
	}
	info, err := state.file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < 0 {
		return 0, outputcap.ErrUnsafeNamespace
	}
	return uint64(info.Size()), nil
}
func (state *fileState) SameFile(other outputcap.FileIdentity) (bool, error) {
	right, ok := stateFrom(other)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(state.verify(), right.verify()); err != nil {
		return false, err
	}
	return state.object == right.object, nil
}
func (state *fileState) ReadAt(data []byte, offset int64) (int, error) {
	if err := state.verify(); err != nil {
		return 0, err
	}
	return state.file.ReadAt(data, offset)
}
func (state *fileState) WriteAt(data []byte, offset int64) (int, error) {
	if err := state.verify(); err != nil {
		return 0, err
	}
	return state.file.WriteAt(data, offset)
}
func (state *fileState) Sync() error {
	if err := state.verify(); err != nil {
		return err
	}
	return state.file.Sync()
}
func (state *fileState) SetModifiedTime(value catalog.ModifiedTime) error {
	if err := state.verify(); err != nil {
		return err
	}
	return setModifiedTime(int(state.file.Fd()), value)
}
func (state *fileState) MetadataMatches(size uint64, value catalog.ModifiedTime) (bool, error) {
	if err := state.verify(); err != nil {
		return false, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(state.file.Fd()), &stat); err != nil {
		return false, nativeError(err)
	}
	return stat.Size >= 0 && uint64(stat.Size) == size &&
		(!value.Present() || stat.Mtim.Sec == value.Seconds() && stat.Mtim.Nsec == int64(value.Nanoseconds())), nil
}

func (dir *directory) openFile(name string, private bool, flags int) (_ *fileState, resultErr error) {
	if err := errors.Join(dir.verify(), validComponent(name)); err != nil {
		return nil, err
	}
	mode := uint32(publicFileMode)
	if private {
		mode = privateFileMode
	}
	fd, err := unix.Openat(dir.fd(), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, nativeError(err)
	}
	state := &fileState{file: os.NewFile(uintptr(fd), name), name: name, private: private}
	defer func() {
		if resultErr == nil {
			return
		}
		if flags&unix.O_EXCL != 0 && state.object.kind == unix.S_IFREG {
			// Rollback uses the created handle, even if private-ACL validation failed.
			state.private = false
			resultErr = errors.Join(resultErr, dir.RemoveFile(name, &mutableFile{State: state}))
		}
		resultErr = errors.Join(resultErr, state.Close())
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, nativeError(err)
	}
	state.object = identity(stat)
	parent, err := dir.Duplicate()
	if err != nil {
		return nil, err
	}
	state.parent = parent.(*directory)
	if state.object.device != dir.volume.device {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err := state.verify(); err != nil {
		return nil, err
	}
	return state, nil
}
func (dir *directory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	if size < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err := dir.ValidateCreateAuthority(); err != nil {
		return nil, err
	}
	state, err := dir.openFile(name, private, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	file := &mutableFile{State: state}
	if err := state.file.Truncate(size); err != nil {
		return nil, errors.Join(err, dir.RemoveFile(name, file), file.Close())
	}
	return file, nil
}
func (dir *directory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	state, err := dir.openFile(name, private, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return &observedFile{State: state}, nil
}
func (dir *directory) OpenRecoveryDurabilityFile(name string, private bool) (outputcap.RecoveryDurabilityFile, error) {
	state, err := dir.openFile(name, private, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return &recoveryFile{State: state}, nil
}
func (dir *directory) OpenMutableFile(name string, private bool) (outputcap.MutableFile, error) {
	state, err := dir.openFile(name, private, unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	return &mutableFile{State: state}, nil
}
