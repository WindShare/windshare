//go:build darwin

// Package outputdarwin supplies anchored namespace and identity primitives.
// File lifecycles, checkpoints, recovery and cleanup decisions remain shared.
package outputdarwin

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	privateDirectoryMode = 0o700
	publicDirectoryMode  = 0o755
	privateFileMode      = 0o600
	publicFileMode       = 0o666
	directoryOpenFlags   = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
)

type Platform struct {
	root        *directory
	path        string
	disposition outputcap.RootOpenDisposition
}

func Open(path string, create bool) (*Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	root, created, err := openRoot(path, create)
	if err != nil {
		return nil, err
	}
	disposition := outputcap.CallerProvidedContainer
	if created {
		disposition = outputcap.AuthorityCreatedRoot
	}
	return &Platform{root: root, path: path, disposition: disposition}, nil
}

// Resolve the initial destination spelling (including macOS's /var and /tmp
// aliases), then pin every component. All subsequent mutation is handle-relative.
func openRoot(path string, create bool) (*directory, bool, error) {
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !create || !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
		parent, _, err := openRoot(filepath.Dir(path), true)
		if err != nil {
			return nil, false, err
		}
		child, err := parent.CreateDirectory(filepath.Base(path), false)
		closeErr := parent.Close()
		if err != nil {
			return nil, false, errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return nil, false, errors.Join(closeErr, child.Close())
		}
		return child.(*directory), true, nil
	}
	fd, err := unix.Open("/", directoryOpenFlags, 0)
	if err != nil {
		return nil, false, nativeError(err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(physical, "/"), "/") {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, directoryOpenFlags, 0)
		closeErr := unix.Close(fd)
		if openErr != nil {
			return nil, false, errors.Join(nativeError(openErr), closeErr)
		}
		if closeErr != nil {
			return nil, false, errors.Join(closeErr, unix.Close(next))
		}
		fd = next
	}
	volume, err := inspectVolume(fd)
	if err != nil {
		return nil, false, errors.Join(err, unix.Close(fd))
	}
	root, err := newDirectory(fd, volume, nil, "", false)
	return root, false, err
}

func (platform *Platform) Root() outputcap.Directory { return platform.root }
func (platform *Platform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return platform.disposition
}
func (platform *Platform) Close() error {
	if platform == nil || platform.root == nil {
		return nil
	}
	err := platform.root.Close()
	platform.root = nil
	return err
}

type placementGuard struct{ root *directory }

func (guard *placementGuard) Root() outputcap.Directory { return guard.root }
func (guard *placementGuard) Close() error              { return guard.root.Close() }

func (platform *Platform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	if platform == nil || platform.root == nil {
		return nil, os.ErrClosed
	}
	current, _, err := openRoot(platform.path, false)
	if err != nil {
		return nil, err
	}
	same, err := current.SameDirectory(platform.root)
	if err == nil && same {
		err = current.ValidateCreateAuthority()
	}
	if err != nil || !same {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, current.Close())
	}
	return &placementGuard{root: current}, nil
}

func (platform *Platform) IdentityScheme() outputcap.IdentitySchemeID {
	if platform.root == nil || !platform.root.volume.restart {
		return ""
	}
	return outputcap.IdentitySchemeDarwinObjectIDV1
}
func (platform *Platform) Durability() transfer.DurabilityLevel {
	if platform.IdentityScheme() == "" {
		return transfer.DurabilityNone
	}
	// fsync establishes ordering across process termination. No power-loss
	// promise is inferred from a successful call or from the APFS name.
	return transfer.DurabilityProcessRestart
}
func (*Platform) ValidateModifiedTime(value catalog.ModifiedTime) error {
	return validateModifiedTime(value)
}
func (platform *Platform) CanonicalComponentKey(name string) (string, error) {
	if err := validComponent(name); err != nil {
		return "", err
	}
	key := norm.NFD.String(name)
	if !platform.root.volume.caseSensitive {
		key = cases.Fold().String(key)
	}
	return key, nil
}
func (platform *Platform) CanonicalLocatorKey(path string) (string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return "", errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i], err = platform.CanonicalComponentKey(part)
		if err != nil {
			return "", err
		}
	}
	return strings.Join(parts, "/"), nil
}

func (platform *Platform) DestinationCapabilities() (result outputcap.DestinationCapabilities, resultErr error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	workspace, err := outputprobe.Allocate(guard.Root(), rand.Reader)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, workspace.Cleanup()) }()
	facts := outputprobe.Results{CrashCleanup: outputcap.ErrRecoverableOutputUnsupported}
	facts.SafePublish = workspace.SafePublish(outputprobe.LinuxExt4)
	if err := workspace.Clear(); err != nil {
		return result, err
	}
	// A live namespace probe cannot establish a persistent identity.
	if platform.IdentityScheme() == "" {
		facts.OperationRecovery, facts.RangeRecovery = outputcap.ErrRecoverableOutputUnsupported, outputcap.ErrRecoverableOutputUnsupported
		return facts.Capabilities()
	}
	if _, err := platform.root.PersistentDirectoryIdentityClaim(); err != nil {
		return result, err
	}
	facts.OperationRecovery = workspace.OperationRecovery()
	if err := workspace.Clear(); err != nil {
		return result, err
	}
	facts.RangeRecovery = workspace.RangeRecovery()
	if err := workspace.Clear(); err != nil {
		return result, err
	}
	lock, _, err := workspace.Directory().AcquireLock("probe.lock", false)
	if err != nil {
		return result, err
	}
	contender, _, lockErr := workspace.Directory().AcquireLock("probe.lock", true)
	if contender != nil {
		_ = contender.Close()
	}
	if !errors.Is(lockErr, outputcap.ErrNamespaceLockBusy) {
		_ = lock.Close()
		return result, errors.Join(outputcap.ErrRecoverableOutputUnsupported, lockErr)
	}
	// Remove while the handle still pins the exact lock object.
	removeErr := workspace.Directory().RemoveFile("probe.lock", lock.File())
	if err := errors.Join(removeErr, lock.Close()); err != nil {
		return result, err
	}
	return facts.Capabilities()
}

var _ outputcap.Platform = (*Platform)(nil)
