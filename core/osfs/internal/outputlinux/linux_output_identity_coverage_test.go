//go:build linux

package outputlinux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

type linuxIdentityCoverageProvider struct {
	prepare func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error)
	read    func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error)
}

func (provider linuxIdentityCoverageProvider) Prepare(
	system *linuxOutputSystem,
	fd int,
	mount linuxMountIdentity,
) (linuxDirectoryRestartIdentity, error) {
	return provider.prepare(system, fd, mount)
}

func (provider linuxIdentityCoverageProvider) Read(
	system *linuxOutputSystem,
	fd int,
	mount linuxMountIdentity,
) (linuxDirectoryRestartIdentity, error) {
	return provider.read(system, fd, mount)
}

func TestLinuxIdentityCoverageSeparatesLiveHandleAndRestartEvidence(t *testing.T) {
	live := linuxOpenHandleIdentity{
		mountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
		deviceMinor: linuxTestDeviceMinor, inode: linuxTestRootInode, kind: unix.S_IFDIR,
	}
	legacy := live
	legacy.mountID = linuxTestLegacyMountID
	if live.sameObject(legacy) {
		t.Fatal("live identity accepted a mount-ID-domain change")
	}
	if !live.sameInodeObject(legacy) {
		t.Fatal("certification could not compare one pinned inode across legacy and unique mount-ID domains")
	}
	for name, mutate := range map[string]func(*linuxOpenHandleIdentity){
		"device major": func(identity *linuxOpenHandleIdentity) { identity.deviceMajor++ },
		"device minor": func(identity *linuxOpenHandleIdentity) { identity.deviceMinor++ },
		"inode":        func(identity *linuxOpenHandleIdentity) { identity.inode++ },
		"kind":         func(identity *linuxOpenHandleIdentity) { identity.kind = unix.S_IFREG },
	} {
		t.Run(name, func(t *testing.T) {
			changed := legacy
			mutate(&changed)
			if live.sameInodeObject(changed) {
				t.Fatalf("pinned-inode comparison ignored %s", name)
			}
		})
	}
	if !(linuxOpenHandleFacts{identity: live}).matches(live) ||
		!(linuxNamedEntrySnapshot{identity: live}).matches(live) {
		t.Fatal("handle-bound witnesses rejected their pinned object")
	}

	restart := linuxIdentityCoverageRestart(linuxTestMountIdentity())
	if !restart.matchesHandle(live) {
		t.Fatal("restart identity did not bind back to its live handle")
	}
	reused := restart
	reused.birthNanoseconds++
	if !reused.matchesHandle(live) {
		t.Fatal("restart-only incarnation evidence leaked into current-handle comparison")
	}
	if restart.sameDirectory(reused) {
		t.Fatal("restart comparison accepted an inode reuse with a different birth time")
	}
	reused = restart
	reused.mount.filesystemUUID[0]++
	if !reused.matchesHandle(live) {
		t.Fatal("persistent filesystem evidence leaked into current-handle comparison")
	}
	if restart.sameDirectory(reused) {
		t.Fatal("restart comparison ignored persistent filesystem identity")
	}
}

func TestLinuxIdentityCoverageStatxRestartFailureBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string, *unix.Statx_t) error
		want   error
		cause  error
	}{
		{
			name: "initial handle read",
			mutate: func(stage string, _ *unix.Statx_t) error {
				if stage == "before" {
					return unix.EIO
				}
				return nil
			},
			want: unix.EIO,
		},
		{
			name: "initial handle is not a directory",
			mutate: func(stage string, stat *unix.Statx_t) error {
				if stage == "before" {
					stat.Mode = unix.S_IFREG | 0o600
				}
				return nil
			},
			want: errLinuxOutputUnsafe,
		},
		{
			name: "initial handle escaped mount",
			mutate: func(stage string, stat *unix.Statx_t) error {
				if stage == "before" {
					stat.Dev_minor++
				}
				return nil
			},
			want: errLinuxOutputUnsafe,
		},
		{
			name: "birth observation unsupported",
			mutate: func(stage string, _ *unix.Statx_t) error {
				if stage == "birth" {
					return unix.EOPNOTSUPP
				}
				return nil
			},
			want: errLinuxOutputUnsupported, cause: unix.EOPNOTSUPP,
		},
		{
			name: "birth observation syscall failure",
			mutate: func(stage string, _ *unix.Statx_t) error {
				if stage == "birth" {
					return unix.EIO
				}
				return nil
			},
			want: unix.EIO,
		},
		{
			name: "birth observation names another object",
			mutate: func(stage string, stat *unix.Statx_t) error {
				if stage == "birth" {
					stat.Ino++
				}
				return nil
			},
			want: errLinuxOutputUnsafe,
		},
		{
			name: "invalid birth nanoseconds",
			mutate: func(stage string, stat *unix.Statx_t) error {
				if stage == "birth" {
					stat.Btime.Nsec = 1_000_000_000
				}
				return nil
			},
			want: errLinuxOutputUnsafe,
		},
		{
			name: "final handle read",
			mutate: func(stage string, _ *unix.Statx_t) error {
				if stage == "after" {
					return unix.EIO
				}
				return nil
			},
			want: unix.EIO,
		},
		{
			name: "handle changed after observation",
			mutate: func(stage string, stat *unix.Statx_t) error {
				if stage == "after" {
					stat.Ino++
				}
				return nil
			},
			want: errLinuxOutputUnsafe,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := linuxCertificationTestSystem(
				linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
			)
			original := system.statx
			handleReads := 0
			system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
				if err := original(fd, path, flags, mask, stat); err != nil {
					return err
				}
				stage := "birth"
				if mask&unix.STATX_BTIME == 0 {
					handleReads++
					stage = "before"
					if handleReads == 2 {
						stage = "after"
					}
				}
				return test.mutate(stage, stat)
			}

			_, err := linuxReadStatxBirthTimeRestartIdentity(&system, 17, linuxTestMountIdentity())
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want classification %v", err, test.want)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("error %v did not retain cause %v", err, test.cause)
			}
		})
	}
}

func TestLinuxIdentityCoverageStatxProviderUsesReadOnlyObservationForBothModes(t *testing.T) {
	system := linuxCertificationTestSystem(
		linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
	)
	provider := linuxStatxBirthTimeRestartIdentityProvider{}
	prepared, err := provider.Prepare(&system, 17, linuxTestMountIdentity())
	if err != nil {
		t.Fatalf("prepare identity: %v", err)
	}
	read, err := provider.Read(&system, 17, linuxTestMountIdentity())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if !prepared.sameDirectory(read) || !prepared.hasGenerationProof || prepared.generation != linuxTestGeneration {
		t.Fatalf("provider modes returned different restart evidence: prepare=%+v read=%+v", prepared, read)
	}
}

func TestLinuxIdentityCoverageOptionalGenerationTaxonomy(t *testing.T) {
	tests := []struct {
		name       string
		get        func(int) (uint32, error)
		generation uint32
		proof      bool
		wantError  bool
	}{
		{name: "provider absent"},
		{name: "ioctl inappropriate", get: func(int) (uint32, error) { return 0, unix.ENOTTY }},
		{name: "kernel omitted ioctl", get: func(int) (uint32, error) { return 0, unix.ENOSYS }},
		{name: "filesystem omitted ioctl", get: func(int) (uint32, error) { return 0, unix.EOPNOTSUPP }},
		{name: "zero carries no proof", get: func(int) (uint32, error) { return 0, nil }},
		{name: "nonzero is additive proof", get: func(int) (uint32, error) { return 37, nil }, generation: 37, proof: true},
		{name: "unexpected failure is explicit", get: func(int) (uint32, error) { return 0, unix.EIO }, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := linuxOutputSystem{getVersion: test.get}
			generation, proof, err := linuxReadOptionalInodeGeneration(&system, 19, "test restart identity")
			if test.wantError {
				if !errors.Is(err, errLinuxOutputUnsupported) || !errors.Is(err, unix.EIO) {
					t.Fatalf("unexpected generation error classification: %v", err)
				}
				return
			}
			if err != nil || generation != test.generation || proof != test.proof {
				t.Fatalf("generation=(%d,%t), error=%v; want (%d,%t)",
					generation, proof, err, test.generation, test.proof)
			}
		})
	}
}

func TestLinuxIdentityCoverageStableClaimEncodingAndValidation(t *testing.T) {
	mount := linuxTestMountIdentity()
	encodedMount, err := linuxEncodeMountIdentity(mount)
	if err != nil {
		t.Fatalf("encode mount: %v", err)
	}
	if want := linuxIdentityCoverageExpectedMountClaim(mount); !bytes.Equal(encodedMount, want) {
		t.Fatalf("mount claim = %x, want %x", encodedMount, want)
	}
	for name, malformed := range map[string]linuxMountIdentity{
		"zero unique mount ID": func() linuxMountIdentity { value := mount; value.uniqueMountID = 0; return value }(),
		"zero filesystem UUID": func() linuxMountIdentity {
			value := mount
			value.filesystemUUID = [linuxFilesystemUUIDBytes]byte{}
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := linuxEncodeMountIdentity(malformed); !errors.Is(err, errLinuxOutputUnsupported) {
				t.Fatalf("malformed mount was accepted: %v", err)
			}
		})
	}

	identity := linuxIdentityCoverageRestart(mount)
	encoded, err := linuxEncodeDirectoryRestartIdentity(identity)
	if err != nil {
		t.Fatalf("encode directory: %v", err)
	}
	if want := linuxIdentityCoverageExpectedDirectoryClaim(identity, encodedMount); !bytes.Equal(encoded, want) {
		t.Fatalf("directory claim = %x, want %x", encoded, want)
	}
	absentGeneration := identity
	absentGeneration.generation = 0
	absentGeneration.hasGenerationProof = false
	encoded, err = linuxEncodeDirectoryRestartIdentity(absentGeneration)
	if err != nil {
		t.Fatalf("encode identity without optional generation: %v", err)
	}
	if want := linuxIdentityCoverageExpectedDirectoryClaim(absentGeneration, encodedMount); !bytes.Equal(encoded, want) {
		t.Fatalf("generation-absent claim = %x, want %x", encoded, want)
	}

	malformed := map[string]linuxDirectoryRestartIdentity{}
	value := identity
	value.kind = unix.S_IFREG
	malformed["not a directory"] = value
	value = identity
	value.inode = 0
	malformed["zero inode"] = value
	value = identity
	value.birthNanoseconds = 1_000_000_000
	malformed["invalid birth nanoseconds"] = value
	value = identity
	value.generation = 0
	malformed["claimed zero generation"] = value
	value = identity
	value.hasGenerationProof = false
	malformed["unclaimed nonzero generation"] = value
	value = identity
	value.mount.filesystemUUID = [linuxFilesystemUUIDBytes]byte{}
	malformed["incomplete mount"] = value
	for name, candidate := range malformed {
		t.Run(name, func(t *testing.T) {
			if _, err := linuxEncodeDirectoryRestartIdentity(candidate); !errors.Is(err, errLinuxOutputUnsupported) {
				t.Fatalf("malformed restart identity was accepted: %v", err)
			}
		})
	}
}

func TestLinuxIdentityCoverageReadOpenHandleFactsErrorTaxonomy(t *testing.T) {
	if _, err := linuxReadOpenHandleFacts(nil, 23, unix.STATX_MNT_ID_UNIQUE); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("nil syscall provider classification: %v", err)
	}
	if _, err := linuxReadOpenHandleFacts(&linuxOutputSystem{}, 23, unix.STATX_MNT_ID_UNIQUE); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("absent statx provider classification: %v", err)
	}
	for _, failure := range []error{unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP} {
		system := linuxOutputSystem{statx: func(int, string, int, int, *unix.Statx_t) error { return failure }}
		_, err := linuxReadOpenHandleFacts(&system, 23, unix.STATX_MNT_ID_UNIQUE)
		if !errors.Is(err, errLinuxOutputUnsupported) || !errors.Is(err, failure) {
			t.Fatalf("statx failure %v classified as %v", failure, err)
		}
	}
	system := linuxOutputSystem{statx: func(int, string, int, int, *unix.Statx_t) error { return unix.EIO }}
	if _, err := linuxReadOpenHandleFacts(&system, 23, unix.STATX_MNT_ID_UNIQUE); !errors.Is(err, unix.EIO) {
		t.Fatalf("unexpected statx failure lost its cause: %v", err)
	}
	system.statx = func(_ int, _ string, _ int, mask int, stat *unix.Statx_t) error {
		*stat = unix.Statx_t{Mask: uint32(mask) &^ uint32(unix.STATX_UID)}
		return nil
	}
	if _, err := linuxReadOpenHandleFacts(&system, 23, unix.STATX_MNT_ID_UNIQUE); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("incomplete statx result classification: %v", err)
	}
}

func TestLinuxIdentityCoverageCertificationUsesRecoveryIdentity(t *testing.T) {
	t.Run("recovery read only", func(t *testing.T) {
		system := linuxCertificationTestSystem(
			linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
		)
		readCalls := 0
		system.restartIdentity = linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				t.Fatal("filesystem certification attempted enrollment")
				return linuxDirectoryRestartIdentity{}, nil
			},
			read: func(_ *linuxOutputSystem, _ int, mount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				readCalls++
				return linuxIdentityCoverageRestart(mount), nil
			},
		}
		certificate, err := linuxCertifyExt4OutputFD(&system, 31)
		if err != nil {
			t.Fatalf("certify output identity: %v", err)
		}
		if readCalls != 1 || !certificate.rootRestartIdentity.matchesHandle(certificate.rootObject) {
			t.Fatalf("certification did not retain one recovery observation: reads=%d certificate=%+v",
				readCalls, certificate)
		}
	})

	t.Run("restart identity differs from live root", func(t *testing.T) {
		system := linuxCertificationTestSystem(
			linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
		)
		system.restartIdentity = linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, errors.New("unexpected prepare")
			},
			read: func(_ *linuxOutputSystem, _ int, mount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				identity := linuxIdentityCoverageRestart(mount)
				identity.inode++
				return identity, nil
			},
		}
		if _, err := linuxCertifyExt4OutputFD(&system, 31); !errors.Is(err, errLinuxOutputUnsafe) {
			t.Fatalf("certification accepted mismatched restart identity: %v", err)
		}
	})

	t.Run("root swapped between mount ID observations", func(t *testing.T) {
		system := linuxCertificationTestSystem(
			linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
		)
		original := system.statx
		system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
			if err := original(fd, path, flags, mask, stat); err != nil {
				return err
			}
			if mask&unix.STATX_MNT_ID_UNIQUE != 0 {
				stat.Ino++
			}
			return nil
		}
		if _, err := linuxCertifyExt4OutputFD(&system, 31); !errors.Is(err, errLinuxOutputUnsafe) {
			t.Fatalf("certification accepted a root swap: %v", err)
		}
	})

	t.Run("provider failure remains attributable", func(t *testing.T) {
		system := linuxCertificationTestSystem(
			linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
		)
		failure := errors.New("restart identity read failed")
		system.restartIdentity = linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, errors.New("unexpected prepare")
			},
			read: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, failure
			},
		}
		if _, err := linuxCertifyExt4OutputFD(&system, 31); !errors.Is(err, failure) {
			t.Fatalf("certification lost provider failure: %v", err)
		}
	})
}

func TestLinuxIdentityCoverageRootBindingFailureTaxonomy(t *testing.T) {
	var closed *linuxV3Platform
	if _, err := closed.RootBinding(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed platform classification: %v", err)
	}

	mount := linuxTestMountIdentity()
	current := linuxOpenHandleIdentity{
		mountID: mount.uniqueMountID, deviceMajor: mount.deviceMajor,
		deviceMinor: mount.deviceMinor, inode: linuxTestRootInode, kind: unix.S_IFDIR,
	}
	restart := linuxIdentityCoverageRestart(mount)
	readProvider := func(read func(linuxMountIdentity) (linuxDirectoryRestartIdentity, error)) linuxDirectoryRestartIdentityProvider {
		return linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, errors.New("unexpected prepare")
			},
			read: func(_ *linuxOutputSystem, _ int, observedMount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return read(observedMount)
			},
		}
	}

	t.Run("incomplete persistent mount", func(t *testing.T) {
		provider := readProvider(func(linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
			return restart, nil
		})
		directory := linuxIdentityCoverageDirectory(&current, restart, provider)
		directory.certificate.mount.filesystemUUID = [linuxFilesystemUUIDBytes]byte{}
		platform := &linuxV3Platform{root: &linuxV3Directory{native: directory}}
		if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
			t.Fatalf("incomplete mount classification: %v", err)
		}
	})

	t.Run("provider absent", func(t *testing.T) {
		directory := linuxIdentityCoverageDirectory(&current, restart, nil)
		platform := &linuxV3Platform{root: &linuxV3Directory{native: directory}}
		if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
			t.Fatalf("absent provider classification: %v", err)
		}
	})

	t.Run("provider failure", func(t *testing.T) {
		failure := errors.New("persistent identity unavailable")
		provider := readProvider(func(linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
			return linuxDirectoryRestartIdentity{}, failure
		})
		directory := linuxIdentityCoverageDirectory(&current, restart, provider)
		platform := &linuxV3Platform{root: &linuxV3Directory{native: directory}}
		if _, err := platform.RootBinding(); !errors.Is(err, failure) {
			t.Fatalf("provider failure lost its cause: %v", err)
		}
	})

	t.Run("persistent identity names another live object", func(t *testing.T) {
		provider := readProvider(func(observedMount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
			identity := linuxIdentityCoverageRestart(observedMount)
			identity.inode++
			return identity, nil
		})
		directory := linuxIdentityCoverageDirectory(&current, restart, provider)
		platform := &linuxV3Platform{root: &linuxV3Directory{native: directory}}
		if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("mismatched persistent identity classification: %v", err)
		}
	})

	t.Run("malformed persistent identity", func(t *testing.T) {
		malformed := restart
		malformed.birthNanoseconds = 1_000_000_000
		provider := readProvider(func(linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
			return malformed, nil
		})
		directory := linuxIdentityCoverageDirectory(&current, malformed, provider)
		platform := &linuxV3Platform{root: &linuxV3Directory{native: directory}}
		if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
			t.Fatalf("malformed restart claim classification: %v", err)
		}
	})
}

func TestLinuxIdentityCoverageDirectoryClaimFailureTaxonomy(t *testing.T) {
	mount := linuxTestMountIdentity()
	current := linuxOpenHandleIdentity{
		mountID: mount.uniqueMountID, deviceMajor: mount.deviceMajor,
		deviceMinor: mount.deviceMinor, inode: linuxTestRootInode, kind: unix.S_IFDIR,
	}
	restart := linuxIdentityCoverageRestart(mount)

	t.Run("provider absent", func(t *testing.T) {
		directory := linuxIdentityCoverageDirectory(&current, restart, nil)
		if _, err := directory.identityClaim(); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("absent provider classification: %v", err)
		}
	})

	t.Run("provider failure", func(t *testing.T) {
		failure := errors.New("read failed")
		provider := linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, errors.New("unexpected prepare")
			},
			read: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, failure
			},
		}
		directory := linuxIdentityCoverageDirectory(&current, restart, provider)
		if _, err := directory.identityClaim(); !errors.Is(err, failure) {
			t.Fatalf("provider failure lost its cause: %v", err)
		}
	})

	t.Run("restart identity names another handle", func(t *testing.T) {
		provider := linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, errors.New("unexpected prepare")
			},
			read: func(_ *linuxOutputSystem, _ int, observedMount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				identity := linuxIdentityCoverageRestart(observedMount)
				identity.inode++
				return identity, nil
			},
		}
		directory := linuxIdentityCoverageDirectory(&current, restart, provider)
		if _, err := directory.identityClaim(); !errors.Is(err, errLinuxOutputUnsafe) {
			t.Fatalf("mismatched handle classification: %v", err)
		}
	})

	t.Run("malformed encoded identity", func(t *testing.T) {
		malformed := restart
		malformed.generation = 0
		provider := linuxIdentityCoverageProvider{
			prepare: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return linuxDirectoryRestartIdentity{}, errors.New("unexpected prepare")
			},
			read: func(*linuxOutputSystem, int, linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
				return malformed, nil
			},
		}
		directory := linuxIdentityCoverageDirectory(&current, malformed, provider)
		if _, err := directory.identityClaim(); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("malformed identity classification: %v", err)
		}
	})
}

func TestLinuxIdentityCoverageEnrollmentAndRecoveryUseDistinctProviderModes(t *testing.T) {
	mount := linuxTestMountIdentity()
	current := linuxOpenHandleIdentity{
		mountID: mount.uniqueMountID, deviceMajor: mount.deviceMajor,
		deviceMinor: mount.deviceMinor, inode: linuxTestRootInode, kind: unix.S_IFDIR,
	}
	observed := linuxIdentityCoverageRestart(mount)
	prepareCalls := 0
	readCalls := 0
	provider := linuxIdentityCoverageProvider{
		prepare: func(_ *linuxOutputSystem, _ int, gotMount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
			prepareCalls++
			if gotMount != mount {
				t.Fatalf("prepare mount = %+v, want %+v", gotMount, mount)
			}
			return observed, nil
		},
		read: func(_ *linuxOutputSystem, _ int, gotMount linuxMountIdentity) (linuxDirectoryRestartIdentity, error) {
			readCalls++
			if gotMount != mount {
				t.Fatalf("read mount = %+v, want %+v", gotMount, mount)
			}
			return observed, nil
		},
	}
	directory := linuxIdentityCoverageDirectory(&current, observed, provider)
	prepared, err := directory.prepareIdentityClaim()
	if err != nil {
		t.Fatalf("prepare identity claim: %v", err)
	}
	recovered, err := directory.identityClaim()
	if err != nil {
		t.Fatalf("read identity claim: %v", err)
	}
	if !bytes.Equal(prepared, recovered) || prepareCalls != 1 || readCalls != 1 {
		t.Fatalf("claims differ or wrong provider mode was used: prepare=%d read=%d", prepareCalls, readCalls)
	}

	platform := &linuxV3Platform{root: &linuxV3Directory{native: directory}}
	first, err := platform.RootBinding()
	if err != nil || first.IsZero() {
		t.Fatalf("root binding: value=%v error=%v", first, err)
	}
	second, err := platform.RootBinding()
	if err != nil || first != second || prepareCalls != 1 || readCalls != 3 {
		t.Fatalf("root binding was not stable/read-only: equal=%t prepare=%d read=%d error=%v",
			first == second, prepareCalls, readCalls, err)
	}

	// Birth time is intentionally absent from the live handle identity. Recovery
	// must therefore reject its change through the persistent identity comparison.
	observed.birthNanoseconds++
	if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("root binding accepted changed restart evidence: %v", err)
	}
	if _, err := directory.identityClaim(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("directory claim accepted changed restart evidence: %v", err)
	}

	observed = directory.certificate.rootRestartIdentity
	current.inode++
	readsBefore := readCalls
	if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("root binding accepted changed live handle identity: %v", err)
	}
	if readCalls != readsBefore {
		t.Fatal("persistent identity was read after live-handle revalidation had already failed")
	}
}

func linuxIdentityCoverageRestart(mount linuxMountIdentity) linuxDirectoryRestartIdentity {
	return linuxDirectoryRestartIdentity{
		mount: mount, inode: linuxTestRootInode, kind: unix.S_IFDIR,
		birthSeconds: linuxTestBirthSeconds, birthNanoseconds: linuxTestBirthNanos,
		generation: linuxTestGeneration, hasGenerationProof: true,
	}
}

func linuxIdentityCoverageExpectedMountClaim(mount linuxMountIdentity) []byte {
	claim := binary.BigEndian.AppendUint16(nil, uint16(len(linuxMountIdentityClaimDomain)))
	claim = append(claim, linuxMountIdentityClaimDomain...)
	claim = binary.BigEndian.AppendUint64(claim, mount.uniqueMountID)
	claim = binary.BigEndian.AppendUint32(claim, mount.deviceMajor)
	claim = binary.BigEndian.AppendUint32(claim, mount.deviceMinor)
	claim = binary.BigEndian.AppendUint32(claim, uint32(mount.runtimeFilesystemID[0]))
	claim = binary.BigEndian.AppendUint32(claim, uint32(mount.runtimeFilesystemID[1]))
	return append(claim, mount.filesystemUUID[:]...)
}

func linuxIdentityCoverageExpectedDirectoryClaim(
	identity linuxDirectoryRestartIdentity,
	mount []byte,
) []byte {
	claim := binary.BigEndian.AppendUint16(nil, uint16(len(linuxDirectoryRestartIdentityClaimDomain)))
	claim = append(claim, linuxDirectoryRestartIdentityClaimDomain...)
	claim = binary.BigEndian.AppendUint32(claim, uint32(len(mount)))
	claim = append(claim, mount...)
	claim = binary.BigEndian.AppendUint64(claim, identity.inode)
	claim = binary.BigEndian.AppendUint64(claim, uint64(identity.birthSeconds))
	claim = binary.BigEndian.AppendUint32(claim, identity.birthNanoseconds)
	if identity.hasGenerationProof {
		claim = append(claim, 1)
	} else {
		claim = append(claim, 0)
	}
	claim = binary.BigEndian.AppendUint32(claim, identity.generation)
	return binary.BigEndian.AppendUint16(claim, identity.kind)
}

func linuxIdentityCoverageDirectory(
	current *linuxOpenHandleIdentity,
	restart linuxDirectoryRestartIdentity,
	provider linuxDirectoryRestartIdentityProvider,
) *linuxOutputDirectory {
	const receiverUID = uint32(4242)
	mount := restart.mount
	system := &linuxOutputSystem{
		statx: func(_ int, _ string, _ int, mask int, stat *unix.Statx_t) error {
			*stat = unix.Statx_t{
				Mask: uint32(mask), Ino: current.inode, Mode: current.kind | linuxOutputDirectoryMode,
				Dev_major: current.deviceMajor, Dev_minor: current.deviceMinor,
				Mnt_id: current.mountID, Uid: receiverUID,
			}
			return nil
		},
		fstatfs: func(_ int, stat *unix.Statfs_t) error {
			stat.Type = linuxExt4SuperMagic
			stat.Fsid.Val = mount.runtimeFilesystemID
			return nil
		},
		getFlags:        func(int) (uint32, error) { return 0, nil },
		geteuid:         func() int { return int(receiverUID) },
		restartIdentity: provider,
	}
	object := *current
	return &linuxOutputDirectory{
		system: system, fd: 29,
		certificate: linuxOutputCertificate{
			mount: mount, rootObject: object, rootRestartIdentity: restart,
			durability: linuxOutputProcessRestartDurability,
		},
		object: object,
	}
}
