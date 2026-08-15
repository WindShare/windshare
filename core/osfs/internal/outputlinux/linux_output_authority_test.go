//go:build linux

package outputlinux

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxPublicCreateAuthorityUsesActualAccessOnly(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*linuxAuthorityHarness, *linuxOutputSystem)
	}{
		{
			name: "setgid and inherited group",
			configure: func(harness *linuxAuthorityHarness, _ *linuxOutputSystem) {
				harness.directoryMode = uint16(unix.S_IFDIR | unix.S_ISGID | 0o770)
			},
		},
		{
			name:      "default ACL",
			configure: func(_ *linuxAuthorityHarness, _ *linuxOutputSystem) {},
		},
		{
			name:      "named ACL",
			configure: func(_ *linuxAuthorityHarness, _ *linuxOutputSystem) {},
		},
		{
			name: "foreign owner",
			configure: func(harness *linuxAuthorityHarness, _ *linuxOutputSystem) {
				harness.ownerUID = 2000
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, harness := newLinuxAuthorityRoot(t)
			installLinuxSafeAuthorityHarness(root.system)
			test.configure(harness, root.system)
			if err := root.validatePublicCreateAuthority(); err != nil {
				t.Fatalf("actual-access public authority rejected ordinary permissions: %v", err)
			}
		})
	}

	root, _ := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	root.system.faccessat2 = func(int, string, uint32, int) error { return unix.EACCES }
	if err := root.validatePublicCreateAuthority(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("denied public create authority error = %v", err)
	}
}

func TestLinuxPrivateAuthorityRequiresOwnerOnlyControlBoundary(t *testing.T) {
	root, harness := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	root.requireExactPermissions = true
	root.exactPermissions = linuxOutputDirectoryMode
	harness.directoryMode = uint16(unix.S_IFDIR | 0o770)
	if err := root.validatePrivateCreateAuthority(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("shared private-control boundary error = %v", err)
	}

	harness.directoryMode = uint16(unix.S_IFDIR | linuxOutputDirectoryMode)
	harness.ownerUID = 2000
	root.system.geteuid = func() int { return 1000 }
	if err := root.validatePrivateCreateAuthority(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("foreign-owned private-control boundary error = %v", err)
	}
}

func TestLinuxCreateAuthorityUsesHandleBoundEffectiveCredentials(t *testing.T) {
	root, _ := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	called := false
	root.system.faccessat2 = func(fd int, path string, mode uint32, flags int) error {
		called = true
		if fd != root.fd || path != "" || mode != uint32(unix.W_OK|unix.X_OK) ||
			flags != unix.AT_EMPTY_PATH|unix.AT_EACCESS {
			t.Fatalf("faccessat2 fd=%d path=%q mode=%#x flags=%#x", fd, path, mode, flags)
		}
		return nil
	}
	if err := root.validateCreateAuthority(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("effective-credential access check was not called")
	}
}

func TestLinuxMetadataAuthorityRequiresExactEffectiveUID(t *testing.T) {
	root, harness := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	harness.ownerUID = 2000
	root.system.geteuid = func() int { return 1000 }
	if err := root.validateMetadataAuthority(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("foreign directory metadata authority error = %v", err)
	}
	root.system.geteuid = func() int { return int(harness.ownerUID) }
	if err := root.validateMetadataAuthority(); err != nil {
		t.Fatalf("owner metadata authority = %v", err)
	}
}

func TestLinuxExactPrivateAuthorityRejectsForeignOwnedObjects(t *testing.T) {
	root, harness := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	harness.ownerUID = 2000
	harness.directoryMode = uint16(unix.S_IFDIR | linuxOutputDirectoryMode)
	root.exactPermissions = linuxOutputDirectoryMode
	root.requireExactPermissions = true
	root.system.geteuid = func() int { return 1000 }
	if err := root.verifyHandle(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("foreign-owned exact directory verification = %v", err)
	}

	file := &linuxOutputRegularFile{
		system: root.system, fd: linuxAuthorityRegularFileFD, certificate: root.certificate,
		object: linuxOpenHandleIdentity{
			mountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
			deviceMinor: linuxTestDeviceMinor, inode: linuxTestRootInode + 1,
			kind: unix.S_IFREG,
		},
		exactPermissions: linuxOutputStateFileMode, requireExactPermissions: true,
	}
	if err := file.verifyHandle(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("foreign-owned exact file verification = %v", err)
	}

	root.system.geteuid = func() int { return int(harness.ownerUID) }
	if err := root.verifyHandle(); err != nil {
		t.Fatalf("receiver-owned exact directory verification = %v", err)
	}
	if err := file.verifyHandle(); err != nil {
		t.Fatalf("receiver-owned exact file verification = %v", err)
	}
}

func TestLinuxOpenObjectIdentityRequiresOwnerUID(t *testing.T) {
	root, _ := newLinuxAuthorityRoot(t)
	originalStatx := root.system.statx
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		if err := originalStatx(fd, path, flags, mask, stat); err != nil {
			return err
		}
		stat.Mask &^= unix.STATX_UID
		return nil
	}
	if _, err := linuxVerifyOpenObject(root.system, root.fd, root.certificate); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("statx without owner UID error = %v", err)
	}
}

func TestLinuxAuthorityMissingProviderFallbacks(t *testing.T) {
	root, _ := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)

	// nil faccessat2
	root.system.faccessat2 = nil
	if err := root.validatePublicCreateAuthority(); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("nil faccessat2 error = %v, want unsupported", err)
	}

	// faccessat2 returns ENOSYS
	root.system.faccessat2 = func(int, string, uint32, int) error { return unix.ENOSYS }
	if err := root.validatePublicCreateAuthority(); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("ENOSYS faccessat2 error = %v, want unsupported", err)
	}

	// nil geteuid in metadata authority
	installLinuxSafeAuthorityHarness(root.system)
	root.system.geteuid = nil
	if err := root.validateMetadataAuthority(); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("nil geteuid metadata authority error = %v, want unsupported", err)
	}

	// nil geteuid in private authority
	if err := root.validatePrivateAuthority("test"); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("nil geteuid private authority error = %v, want unsupported", err)
	}

	// statx error in ownerUID
	installLinuxSafeAuthorityHarness(root.system)
	root.system.statx = func(int, string, int, int, *unix.Statx_t) error { return unix.EIO }
	if _, err := root.ownerUID(); err == nil {
		t.Fatal("statx EIO succeeded in ownerUID")
	}
}

func installLinuxSafeAuthorityHarness(system *linuxOutputSystem) {
	system.faccessat2 = func(int, string, uint32, int) error { return nil }
	system.geteuid = func() int { return 0 }
}
