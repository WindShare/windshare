//go:build linux

package outputlinux

import (
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

type linuxAuthorityHarness struct {
	directoryMode uint16
	ownerUID      uint32
}

func newLinuxAuthorityRoot(t *testing.T) (*linuxOutputDirectory, *linuxAuthorityHarness) {
	t.Helper()
	const rootFD = 10
	harness := &linuxAuthorityHarness{}
	filesystemID := [2]int32{17, 29}
	system := &linuxOutputSystem{}
	system.statx = func(_ int, _ string, _ int, mask int, stat *unix.Statx_t) error {
		mode := harness.directoryMode
		if mode == 0 {
			mode = uint16(unix.S_IFDIR | 0o755)
		}
		*stat = unix.Statx_t{
			Mask:      uint32(mask),
			Mode:      mode,
			Ino:       linuxTestRootInode,
			Dev_major: linuxTestDeviceMajor,
			Dev_minor: linuxTestDeviceMinor,
			Mnt_id:    linuxTestUniqueMountID,
			Uid:       harness.ownerUID,
			Btime:     unix.StatxTimestamp{Sec: 1_500_000_000},
		}
		return nil
	}
	system.fstatfs = func(_ int, stat *unix.Statfs_t) error {
		reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(linuxExt4SuperMagic)
		stat.Fsid.Val = filesystemID
		return nil
	}
	system.getVersion = func(int) (uint32, error) { return linuxTestGeneration, nil }
	system.getFlags = func(int) (uint32, error) { return 0, nil }
	system.getFilesystemUUID = func(int) ([linuxFilesystemUUIDBytes]byte, error) {
		return linuxTestFilesystemUUID, nil
	}
	system.restartIdentity = linuxStatxBirthTimeRestartIdentityProvider{}
	system.geteuid = func() int { return int(harness.ownerUID) }
	mount := linuxMountIdentity{
		uniqueMountID:       linuxTestUniqueMountID,
		deviceMajor:         linuxTestDeviceMajor,
		deviceMinor:         linuxTestDeviceMinor,
		runtimeFilesystemID: filesystemID,
		filesystemUUID:      linuxTestFilesystemUUID,
	}
	rootObject := linuxOpenHandleIdentity{
		mountID:     linuxTestUniqueMountID,
		deviceMajor: linuxTestDeviceMajor,
		deviceMinor: linuxTestDeviceMinor,
		inode:       linuxTestRootInode,
		kind:        unix.S_IFDIR,
	}
	certificate := linuxOutputCertificate{
		mount:      mount,
		rootObject: rootObject,
		rootRestartIdentity: linuxDirectoryRestartIdentity{
			mount:              mount,
			inode:              linuxTestRootInode,
			kind:               unix.S_IFDIR,
			birthSeconds:       1_500_000_000,
			generation:         linuxTestGeneration,
			hasGenerationProof: true,
		},
		durability: linuxOutputProcessRestartDurability,
	}
	return &linuxOutputDirectory{
		system:      system,
		fd:          rootFD,
		certificate: certificate,
		object:      certificate.rootObject,
	}, harness
}
