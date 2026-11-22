package output

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

const (
	// ControlDirName holds all private WindShare receive state inside the
	// destination container so user output trees stay clean.
	ControlDirName   = ".windshare-tmp"
	dirLockFileName  = ".lock"
	taskLockFileName = "task.lock"
	journalFileName  = "state.log"
	authorityIDFile  = "authority.id"

	probePrefix     = ".windshare-tmp.probe-"
	privateDirMode  = fs.FileMode(0o700)
	publicDirMode   = fs.FileMode(0o755)
	privateFileMode = fs.FileMode(0o600)
	publicFileMode  = fs.FileMode(0o644)
)

type RootDisposition uint8

const (
	CallerProvidedContainer RootDisposition = iota + 1
	AuthorityCreatedRoot
)

type EntryKind uint8

const (
	EntryAbsent EntryKind = iota
	EntryRegularFile
	EntryDirectory
	EntryOther
)

// DestinationCapabilities reports whether the destination supports atomic
// no-replace publication required for safe resumable downloads.
type DestinationCapabilities struct {
	SafePublish             bool
	SafePublishReason       string
	OperationRecovery       bool
	OperationRecoveryReason string
	valid                   bool
}

func (c DestinationCapabilities) Valid() bool { return c.valid }

func (c DestinationCapabilities) ValidateSupport() error {
	if !c.valid {
		return ErrUnsupportedOutput
	}
	if !c.SafePublish || !c.OperationRecovery {
		return ErrUnsupportedOutput
	}
	return nil
}

// Dir is an open, verified directory handle that pins the directory without
// repeatedly opening and closing os.Root on every child syscall.
type Dir struct {
	path   string
	handle *os.File
	info   os.FileInfo
}

func (d *Dir) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

func (d *Dir) Close() error {
	if d == nil || d.handle == nil {
		return nil
	}
	err := d.handle.Close()
	d.handle = nil
	return err
}

func (d *Dir) SameDirectory(other *Dir) bool {
	if d == nil || other == nil || d.info == nil || other.info == nil {
		return false
	}
	return os.SameFile(d.info, other.info)
}

func (d *Dir) Sync() error {
	if d == nil || d.handle == nil {
		return fs.ErrClosed
	}
	return syncDir(d.handle)
}

// OpenContainer opens an existing destination directory (following a final
// symlink/junction once through the kernel) or creates it when create is true.
func OpenContainer(path string, create bool) (*Dir, RootDisposition, error) {
	if path == "" {
		return nil, 0, ErrUnsafeNamespace
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Lstat(abs)
	if create && errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(abs, publicDirMode); err != nil {
			return nil, 0, err
		}
		dir, err := openDirHandle(abs)
		if err != nil {
			return nil, 0, err
		}
		return dir, AuthorityCreatedRoot, nil
	}
	if err != nil {
		return nil, 0, err
	}
	resolved, expected, err := resolveContainerTarget(abs, info, os.Stat)
	if err != nil {
		return nil, 0, err
	}
	dir, err := openVerifiedDirHandle(resolved, expected)
	if err != nil {
		return nil, 0, err
	}
	return dir, CallerProvidedContainer, nil
}

func ValidComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return ErrUnsafeNamespace
	}
	return nil
}

func validName(name string) error {
	if err := ValidComponent(name); err != nil {
		return err
	}
	return validateNativeName(name)
}

func mapCollision(err error) error {
	if errors.Is(err, fs.ErrExist) {
		return errors.Join(ErrNamespaceCollision, err)
	}
	return err
}

// ProbeDestination verifies that the destination filesystem supports atomic
// no-replace file publication using a temporary scratch directory that is
// removed before returning.
func ProbeDestination(root *Dir) (caps DestinationCapabilities, resultErr error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return DestinationCapabilities{}, err
	}
	probeName := probePrefix + hex.EncodeToString(nonce[:])
	if err := mkdirChild(root, probeName, privateDirMode); err != nil {
		return DestinationCapabilities{}, fmt.Errorf("create destination probe: %w", err)
	}
	probeDir, err := openChildDir(root, probeName)
	if err != nil {
		_ = removeChild(root, probeName, true)
		return DestinationCapabilities{}, err
	}
	defer func() {
		closeErr := probeDir.Close()
		removeErr := os.RemoveAll(filepath.Join(root.path, probeName))
		resultErr = errors.Join(resultErr, closeErr, removeErr)
		if resultErr != nil {
			caps = DestinationCapabilities{}
		}
	}()

	src, err := openChildFile(probeDir, "src", os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return DestinationCapabilities{}, err
	}
	if err := src.Close(); err != nil {
		return DestinationCapabilities{}, err
	}
	existing, err := openChildFile(probeDir, "existing", os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return DestinationCapabilities{}, err
	}
	if err := existing.Close(); err != nil {
		return DestinationCapabilities{}, err
	}

	// Verify no-replace fails with collision when target exists.
	collisionErr := renameFileNoReplace(probeDir, "src", probeDir, "existing", false)
	if errors.Is(collisionErr, ErrUnsupportedOutput) {
		return DestinationCapabilities{
			SafePublish:             false,
			SafePublishReason:       "unsafe-publication",
			OperationRecovery:       false,
			OperationRecoveryReason: "unsafe-publication",
			valid:                   true,
		}, nil
	}
	if !errors.Is(collisionErr, ErrNamespaceCollision) && !errors.Is(collisionErr, fs.ErrExist) {
		if collisionErr == nil {
			return DestinationCapabilities{
				SafePublish:             false,
				SafePublishReason:       "unsafe-publication",
				OperationRecovery:       false,
				OperationRecoveryReason: "unsafe-publication",
				valid:                   true,
			}, nil
		}
		return DestinationCapabilities{}, collisionErr
	}

	// Verify no-replace succeeds when target is absent.
	publishErr := renameFileNoReplace(probeDir, "src", probeDir, "published", false)
	if errors.Is(publishErr, ErrUnsupportedOutput) {
		return DestinationCapabilities{
			SafePublish:             false,
			SafePublishReason:       "unsafe-publication",
			OperationRecovery:       false,
			OperationRecoveryReason: "unsafe-publication",
			valid:                   true,
		}, nil
	}
	if publishErr != nil {
		return DestinationCapabilities{}, publishErr
	}

	return DestinationCapabilities{
		SafePublish:             true,
		SafePublishReason:       "none",
		OperationRecovery:       true,
		OperationRecoveryReason: "none",
		valid:                   true,
	}, nil
}

// FileLock holds an exclusive OS-level file lock (flock / LockFileEx).
type FileLock struct {
	file    *os.File
	release func() error
}

func (l *FileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var err error
	if l.release != nil {
		err = l.release()
	}
	err = errors.Join(err, l.file.Close())
	l.file = nil
	l.release = nil
	return err
}

func acquireLockFile(dir *Dir, name string, existingOnly, blocking bool) (*FileLock, bool, error) {
	created := false
	f, err := openChildFile(dir, name, os.O_RDWR, privateFileMode)
	if errors.Is(err, fs.ErrNotExist) && !existingOnly {
		f, err = openChildFile(dir, name, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
		created = err == nil
		if errors.Is(err, fs.ErrExist) {
			f, err = openChildFile(dir, name, os.O_RDWR, privateFileMode)
		}
	}
	if err != nil {
		return nil, false, err
	}
	release, err := lockFile(f, blocking)
	if err != nil {
		return nil, created, errors.Join(err, f.Close())
	}
	return &FileLock{file: f, release: release}, created, nil
}

func setFileModifiedTimeCatalog(f *os.File, modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	return setFileModifiedTime(f, time.Unix(modified.Seconds(), int64(modified.Nanoseconds())))
}

func setDirModifiedTimeCatalog(d *Dir, modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	return setDirModifiedTime(d, time.Unix(modified.Seconds(), int64(modified.Nanoseconds())))
}

func fileMetadataMatches(f *os.File, size uint64, modified catalog.ModifiedTime) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) != size {
		return false, nil
	}
	return !modified.Present() || info.ModTime().Equal(time.Unix(modified.Seconds(), int64(modified.Nanoseconds()))), nil
}
