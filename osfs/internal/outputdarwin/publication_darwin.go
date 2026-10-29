//go:build darwin

package outputdarwin

import (
	"errors"
	"math"
	"os"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func (state *fileState) verifyOrigin() error {
	if err := state.verify(); err != nil {
		return err
	}
	if state.parent == nil {
		return outputcap.ErrFixedLinkSourceChanged
	}
	if err := state.parent.verify(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(state.parent.fd(), state.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return errors.Join(outputcap.ErrFixedLinkSourceChanged, nativeError(err))
	}
	if identity(stat) != state.object {
		return outputcap.ErrFixedLinkSourceChanged
	}
	return nil
}

func (dir *directory) LinkFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	state, ok := stateFrom(source)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.ValidateCreateAuthority(), validComponent(name), state.verifyOrigin()); err != nil {
		return nil, err
	}
	// linkat is atomic no-replace, and preserves both the original inode and its
	// allocation. The protected source namespace is pinned throughout.
	if err := unix.Linkat(state.parent.fd(), state.name, dir.fd(), name, 0); err != nil {
		return nil, nativeError(err)
	}
	linked, err := dir.OpenObservedFile(name, state.private)
	if err != nil {
		return nil, err
	}
	same, err := linked.SameFile(source)
	if err != nil || !same {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, linked.Close())
	}
	return linked, nil
}
func (dir *directory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	state, ok := stateFrom(source)
	if !ok || !dir.private || !state.private {
		return outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.ValidateCreateAuthority(), validComponent(name), state.verifyOrigin()); err != nil {
		return err
	}
	return nativeError(unix.RenameatxNp(state.parent.fd(), state.name, dir.fd(), name, unix.RENAME_NOFOLLOW_ANY))
}
func (dir *directory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	state, ok := stateFrom(expected)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.ValidateCreateAuthority(), validComponent(name)); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dir.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return nativeError(err)
	}
	if err := state.verify(); err != nil {
		return err
	}
	if identity(stat) != state.object {
		return outputcap.ErrUnsafeNamespace
	}
	return nativeError(unix.Unlinkat(dir.fd(), name, 0))
}
func (dir *directory) PublishFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.PublishNoReplaceOutcome, error) {
	published, err := dir.LinkFileNoReplace(source, name)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil {
		return outputcap.PublishNoReplaceIndeterminate, err
	}
	if err := errors.Join(published.Close(), dir.Sync()); err != nil {
		return outputcap.PublishNoReplaceIndeterminate, err
	}
	return outputcap.PublishNoReplaceCommitted, nil
}
func (dir *directory) ReservePublicDirectoryNoReplace(name string) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	created, err := dir.CreateDirectory(name, false)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return nil, outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate, err
	}
	if err := errors.Join(created.Sync(), dir.Sync()); err != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate, errors.Join(err, created.Close())
	}
	return created, outputcap.PublishNoReplaceCommitted, nil
}

func (dir *directory) CreateOrdinaryOutputStage(stageDirectory outputcap.Directory, name string, size uint64) error {
	if size > math.MaxInt64 {
		return outputcap.ErrUnsafeNamespace
	}
	stage, err := dir.CreateProcessStage(stageDirectory, name, int64(size))
	if err != nil {
		return err
	}
	return stage.Close()
}
