//go:build darwin

package outputdarwin

import (
	"errors"
	"fmt"
	"os"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

// Darwin's _WATTR_OK checks KAUTH_VNODE_WRITE_ATTRIBUTES rather than the
// unrelated right to create children. x/sys does not expose this access mask.
const writeAttributesAccess = 1 << 16

func (dir *directory) ValidateCreateAuthority() error {
	// User destinations retain their group and ACL policy. Only our private
	// control namespace requires exclusive ownership, enforced by verify.
	return dir.validateAccess("create/search", unix.W_OK|unix.X_OK)
}

func (dir *directory) ValidateMetadataAuthority() error {
	return dir.validateAccess("metadata", writeAttributesAccess)
}

func (dir *directory) validateAccess(operation string, access uint32) error {
	if err := dir.verify(); err != nil {
		return err
	}
	// The retained descriptor binds "." to the exact directory. Effective
	// credentials let the kernel evaluate ACL grants, denials and group access.
	if err := unix.Faccessat(dir.fd(), ".", access, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("osfs: Darwin directory device=%d inode=%d lacks %s authority: %w",
			dir.object.device, dir.object.inode, operation,
			errors.Join(outputcap.ErrUnsafeNamespace, nativeError(err)))
	}
	return dir.verify()
}

func (dir *directory) establishPrivateAuthority() error {
	if err := dir.verify(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(dir.fd(), &stat); err != nil {
		return nativeError(err)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return outputcap.ErrUnsafeNamespace
	}
	// This cut is only for a newly created, still empty directory. Never
	// "repair" an existing control directory or alter the user's destination.
	if err := setPrivateDirectorySecurity(dir.fd()); err != nil {
		return err
	}
	dir.private = true
	if err := dir.verify(); err != nil {
		return err
	}
	// Inherited ACLs could admit entries before isolation. An unexpected child
	// is not ours to adopt or delete; leave it for explicit inspection.
	_, err := dir.Names(0)
	return err
}
