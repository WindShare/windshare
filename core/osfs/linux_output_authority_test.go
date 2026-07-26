//go:build linux

package osfs

import (
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxCreateAuthorityRejectsPermissionAndCreateModeInheritance(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*linuxSelectionMetadataHarness, *linuxOutputSystem)
		want      error
	}{
		{
			name: "effective access",
			configure: func(_ *linuxSelectionMetadataHarness, system *linuxOutputSystem) {
				system.faccessat2 = func(int, string, uint32, int) error { return unix.EACCES }
			},
			want: errLinuxOutputUnsafe,
		},
		{
			name: "setgid inheritance",
			configure: func(harness *linuxSelectionMetadataHarness, _ *linuxOutputSystem) {
				harness.directoryMode = uint16(unix.S_IFDIR | unix.S_ISGID | 0o770)
			},
			want: errLinuxOutputUnsupported,
		},
		{
			name: "default ACL inheritance",
			configure: func(_ *linuxSelectionMetadataHarness, system *linuxOutputSystem) {
				system.fgetxattr = func(int, string, []byte) (int, error) { return 8, nil }
			},
			want: errLinuxOutputUnsupported,
		},
		{
			name: "append-only directory",
			configure: func(_ *linuxSelectionMetadataHarness, system *linuxOutputSystem) {
				system.getFlags = func(int) (uint32, error) { return linuxFSAppendFlag, nil }
			},
			want: errLinuxOutputUnsupported,
		},
		{
			name: "immutable directory",
			configure: func(_ *linuxSelectionMetadataHarness, system *linuxOutputSystem) {
				system.getFlags = func(int) (uint32, error) { return linuxFSImmutableFlag, nil }
			},
			want: errLinuxOutputUnsupported,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, harness := newLinuxSelectionMetadataRoot(t)
			installLinuxSafeAuthorityHarness(root.system)
			test.configure(harness, root.system)
			if harness.directoryMode != 0 {
				root.object.mode = harness.directoryMode
				root.certificate.rootObject.mode = harness.directoryMode
			}
			if err := root.validateCreateAuthority(); !errors.Is(err, test.want) {
				t.Fatalf("create authority error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLinuxCreateAuthorityUsesHandleBoundEffectiveCredentials(t *testing.T) {
	root, _ := newLinuxSelectionMetadataRoot(t)
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
	root, harness := newLinuxSelectionMetadataRoot(t)
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
	root, harness := newLinuxSelectionMetadataRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	harness.ownerUID = 2000
	harness.directoryMode = uint16(unix.S_IFDIR | linuxOutputDirectoryMode)
	root.object.mode = harness.directoryMode
	root.object.ownerUID = harness.ownerUID
	root.certificate.rootObject = root.object
	root.exactPermissions = linuxOutputDirectoryMode
	root.requireExactPermissions = true
	root.system.geteuid = func() int { return 1000 }
	if err := root.verifyHandle(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("foreign-owned exact directory verification = %v", err)
	}

	file := &linuxOutputRegularFile{
		system: root.system, fd: 11, certificate: root.certificate,
		object: linuxOpenObjectIdentity{
			mountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
			deviceMinor: linuxTestDeviceMinor, inode: linuxTestRootInode + 1,
			mode: uint16(unix.S_IFREG | linuxOutputStateFileMode), ownerUID: harness.ownerUID,
			generation: linuxTestGeneration + 1, hasGeneration: true,
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
	root, _ := newLinuxSelectionMetadataRoot(t)
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

func installLinuxSafeAuthorityHarness(system *linuxOutputSystem) {
	system.faccessat2 = func(int, string, uint32, int) error { return nil }
	system.fgetxattr = func(int, string, []byte) (int, error) { return 0, unix.ENODATA }
	system.geteuid = func() int { return 0 }
}

func TestLinuxAccessACLEffectiveMutationAuthority(t *testing.T) {
	const receiverUID = uint32(1000)
	tests := []struct {
		name       string
		mode       uint16
		acl        []byte
		wantUnsafe bool
	}{
		{name: "private mode", mode: uint16(unix.S_IFDIR | 0o700)},
		{name: "group write only", mode: uint16(unix.S_IFDIR | 0o720)},
		{name: "group execute only", mode: uint16(unix.S_IFDIR | 0o710)},
		{name: "group write and execute", mode: uint16(unix.S_IFDIR | 0o730), wantUnsafe: true},
		{name: "other write and execute", mode: uint16(unix.S_IFDIR | 0o703), wantUnsafe: true},
		{
			name: "foreign named user effective write and execute",
			mode: uint16(unix.S_IFDIR | 0o730),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLNamedUser, 0o3, 2000},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLMask, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
			wantUnsafe: true,
		},
		{
			name: "foreign named user masked to write only",
			mode: uint16(unix.S_IFDIR | 0o720),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLNamedUser, 0o3, 2000},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLMask, 0o2, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
		},
		{
			name: "receiver named user is excluded",
			mode: uint16(unix.S_IFDIR | 0o730),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLNamedUser, 0o3, receiverUID},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLMask, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
		},
		{
			name: "root named user is excluded",
			mode: uint16(unix.S_IFDIR | 0o730),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLNamedUser, 0o3, 0},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLMask, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
		},
		{
			name: "owning group effective write and execute",
			mode: uint16(unix.S_IFDIR | 0o730),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLMask, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
			wantUnsafe: true,
		},
		{
			name: "named group effective write and execute",
			mode: uint16(unix.S_IFDIR | 0o730),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLNamedGroup, 0o3, 3000},
				linuxTestACLEntry{linuxPOSIXACLMask, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
			wantUnsafe: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := linuxOutputSystem{
				fgetxattr: linuxTestAccessACLProvider(test.acl),
			}
			reason, err := linuxExternalChildMutationAuthority(
				&system, 10, test.mode, receiverUID, "test access ACL",
			)
			if err != nil {
				t.Fatal(err)
			}
			if gotUnsafe := reason != ""; gotUnsafe != test.wantUnsafe {
				t.Fatalf("unsafe=%t reason=%q, want unsafe=%t", gotUnsafe, reason, test.wantUnsafe)
			}
		})
	}
}

func TestLinuxAccessACLRejectsMalformedOrModeDivergentState(t *testing.T) {
	tests := []struct {
		name string
		mode uint16
		acl  []byte
	}{
		{
			name: "extended ACL without mask",
			mode: uint16(unix.S_IFDIR | 0o700),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLNamedUser, 0o3, 2000},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
		},
		{
			name: "duplicate owner",
			mode: uint16(unix.S_IFDIR | 0o700),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
		},
		{
			name: "mode mismatch",
			mode: uint16(unix.S_IFDIR | 0o700),
			acl: linuxTestAccessACL(
				linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLGroupObject, 0o3, linuxPOSIXACLUndefinedID},
				linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := linuxOutputSystem{fgetxattr: linuxTestAccessACLProvider(test.acl)}
			_, err := linuxExternalChildMutationAuthority(
				&system, 10, test.mode, 1000, "test malformed access ACL",
			)
			assertLinuxUnsafe(t, err)
		})
	}
}

type linuxTestACLEntry struct {
	tag         uint16
	permissions uint16
	id          uint32
}

func linuxTestAccessACL(entries ...linuxTestACLEntry) []byte {
	encoded := make([]byte, linuxPOSIXACLHeaderBytes+len(entries)*linuxPOSIXACLEntryBytes)
	binary.LittleEndian.PutUint32(encoded, linuxPOSIXACLVersion)
	for index, entry := range entries {
		offset := linuxPOSIXACLHeaderBytes + index*linuxPOSIXACLEntryBytes
		binary.LittleEndian.PutUint16(encoded[offset:], entry.tag)
		binary.LittleEndian.PutUint16(encoded[offset+2:], entry.permissions)
		binary.LittleEndian.PutUint32(encoded[offset+4:], entry.id)
	}
	return encoded
}

func linuxTestAccessACLProvider(acl []byte) func(int, string, []byte) (int, error) {
	return func(_ int, name string, destination []byte) (int, error) {
		if name != linuxAccessACL || len(acl) == 0 {
			return 0, unix.ENODATA
		}
		if destination == nil {
			return len(acl), nil
		}
		return copy(destination, acl), nil
	}
}
