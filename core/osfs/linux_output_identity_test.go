//go:build linux

package osfs

import (
	"bytes"
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestLinuxOpenHandleFactsDoNotConsumeRestartEvidence(t *testing.T) {
	t.Parallel()
	system := linuxCertificationTestSystem(
		linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
	)
	system.getVersion = func(int) (uint32, error) {
		t.Fatal("live handle identity requested inode generation")
		return 0, nil
	}
	facts, err := linuxReadOpenHandleFacts(&system, 10, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		t.Fatal(err)
	}
	if facts.identity.kind != unix.S_IFDIR || facts.identity.inode != linuxTestRootInode {
		t.Fatalf("unexpected live handle facts: %+v", facts)
	}
}

func TestLinuxStatxRestartIdentityRequiresReturnedBirthAndMountMasks(t *testing.T) {
	t.Parallel()
	for name, omitted := range map[string]uint32{
		"birth time":   uint32(unix.STATX_BTIME),
		"unique mount": uint32(unix.STATX_MNT_ID_UNIQUE),
	} {
		name, omitted := name, omitted
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			system := linuxCertificationTestSystem(
				linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
			)
			original := system.statx
			system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
				if err := original(fd, path, flags, mask, stat); err != nil {
					return err
				}
				if mask&unix.STATX_BTIME != 0 {
					stat.Mask &^= omitted
				}
				return nil
			}
			mount := linuxTestMountIdentity()
			_, err := linuxReadStatxBirthTimeRestartIdentity(&system, 10, mount)
			if !errors.Is(err, errLinuxOutputUnsupported) {
				t.Fatalf("missing %s mask was accepted: %v", name, err)
			}
		})
	}
}

func TestLinuxRestartIdentityAcceptsZeroBirthTimeAndZeroGeneration(t *testing.T) {
	t.Parallel()
	system := linuxCertificationTestSystem(
		linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
	)
	original := system.statx
	system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		if err := original(fd, path, flags, mask, stat); err != nil {
			return err
		}
		stat.Btime = unix.StatxTimestamp{}
		return nil
	}
	system.getVersion = func(int) (uint32, error) { return 0, nil }
	identity, err := linuxReadStatxBirthTimeRestartIdentity(&system, 10, linuxTestMountIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if identity.birthSeconds != 0 || identity.birthNanoseconds != 0 || identity.hasGenerationProof {
		t.Fatalf("unexpected zero-valued identity: %+v", identity)
	}
	if _, err := linuxEncodeDirectoryRestartIdentity(identity); err != nil {
		t.Fatalf("encode zero-valued birth time: %v", err)
	}
}

func TestLinuxDirectoryRestartEncodingBindsEveryPersistentField(t *testing.T) {
	t.Parallel()
	base := linuxDirectoryRestartIdentity{
		mount: linuxTestMountIdentity(), inode: linuxTestRootInode, kind: unix.S_IFDIR,
		birthSeconds: linuxTestBirthSeconds, birthNanoseconds: linuxTestBirthNanos,
		generation: linuxTestGeneration, hasGenerationProof: true,
	}
	encodedBase, err := linuxEncodeDirectoryRestartIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*linuxDirectoryRestartIdentity){
		"mount ID":          func(identity *linuxDirectoryRestartIdentity) { identity.mount.uniqueMountID++ },
		"device major":      func(identity *linuxDirectoryRestartIdentity) { identity.mount.deviceMajor++ },
		"device minor":      func(identity *linuxDirectoryRestartIdentity) { identity.mount.deviceMinor++ },
		"runtime fsid low":  func(identity *linuxDirectoryRestartIdentity) { identity.mount.runtimeFilesystemID[0]++ },
		"runtime fsid high": func(identity *linuxDirectoryRestartIdentity) { identity.mount.runtimeFilesystemID[1]++ },
		"filesystem UUID":   func(identity *linuxDirectoryRestartIdentity) { identity.mount.filesystemUUID[0]++ },
		"inode":             func(identity *linuxDirectoryRestartIdentity) { identity.inode++ },
		"birth seconds":     func(identity *linuxDirectoryRestartIdentity) { identity.birthSeconds++ },
		"birth nanoseconds": func(identity *linuxDirectoryRestartIdentity) { identity.birthNanoseconds++ },
		"generation":        func(identity *linuxDirectoryRestartIdentity) { identity.generation++ },
		"generation absent": func(identity *linuxDirectoryRestartIdentity) {
			identity.generation = 0
			identity.hasGenerationProof = false
		},
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := base
			mutate(&changed)
			encoded, err := linuxEncodeDirectoryRestartIdentity(changed)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(encodedBase, encoded) {
				t.Fatalf("%s change did not alter restart identity", name)
			}
		})
	}
}

func TestLinuxFilesystemUUIDIOCTLUsesNativeReadDirectionAndPackedABI(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(linuxFilesystemUUIDResponse{}); got != linuxFilesystemUUIDResponseBytes {
		t.Fatalf("fsuuid2 ABI size = %d, want %d", got, linuxFilesystemUUIDResponseBytes)
	}
	const directionMask = uint(0xe0000000)
	want := uint(unix.FS_IOC_GETFLAGS)&directionMask |
		linuxFilesystemUUIDResponseBytes<<16 | 0x15<<8
	if got := linuxReadSizedIOCTL(0x15, 0, linuxFilesystemUUIDResponseBytes); got != want {
		t.Fatalf("FS_IOC_GETFSUUID = %#x, want %#x", got, want)
	}
}

func linuxTestMountIdentity() linuxMountIdentity {
	return linuxMountIdentity{
		uniqueMountID:       linuxTestUniqueMountID,
		deviceMajor:         linuxTestDeviceMajor,
		deviceMinor:         linuxTestDeviceMinor,
		runtimeFilesystemID: [2]int32{17, 29},
		filesystemUUID:      linuxTestFilesystemUUID,
	}
}
