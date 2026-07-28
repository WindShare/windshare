//go:build linux

package outputlinux

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"golang.org/x/sys/unix"
)

const (
	linuxPlacementTestReceiverUID = uint32(1000)
	linuxPlacementTestMountID     = uint64(77)
	linuxPlacementTestUniqueMount = uint64(7007)
)

type linuxPlacementTestNode struct {
	fd               int
	inode            uint64
	generation       uint32
	birthNanoseconds uint32
	ownerUID         uint32
	mode             uint16
	magic            int64
	acl              []byte
	children         map[string]*linuxPlacementTestNode
}

type linuxPlacementTestHarness struct {
	nodes             map[int]*linuxPlacementTestNode
	root              *linuxPlacementTestNode
	namedObservations map[string]int
	replaceAfterOpen  bool
}

func TestLinuxAbsolutePlacementClaimIsDeterministicAndRestartIdentityBound(t *testing.T) {
	harness, expected := newLinuxPlacementTestHarness()
	system := harness.system()

	first, err := linuxCertifyAbsoluteOutputPlacement("/home/receiver/output", &system, expected)
	if err != nil {
		t.Fatal(err)
	}
	second, err := linuxCertifyAbsoluteOutputPlacement("/home/receiver/output", &system, expected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("unchanged absolute placement produced different claims")
	}

	components, birthNanoseconds, generations := decodeLinuxPlacementClaim(t, first)
	if want := []string{"", "home", "receiver", "output"}; !reflect.DeepEqual(components, want) {
		t.Fatalf("placement components = %v, want %v", components, want)
	}
	if want := []uint32{11, 12, 13, 14}; !reflect.DeepEqual(generations, want) {
		t.Fatalf("placement generations = %v, want %v", generations, want)
	}
	if want := []uint32{11, 12, 13, 14}; !reflect.DeepEqual(birthNanoseconds, want) {
		t.Fatalf("placement birth nanoseconds = %v, want %v", birthNanoseconds, want)
	}
}

func TestLinuxAbsolutePlacementRejectsOrdinaryPrincipalMutationAuthority(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*linuxPlacementTestHarness)
	}{
		{
			name: "sticky shared parent",
			configure: func(harness *linuxPlacementTestHarness) {
				harness.root.children["home"].mode = uint16(unix.S_IFDIR | unix.S_ISVTX | 0o777)
			},
		},
		{
			name: "foreign owner",
			configure: func(harness *linuxPlacementTestHarness) {
				harness.root.children["home"].ownerUID = 2000
			},
		},
		{
			name: "foreign named user ACL",
			configure: func(harness *linuxPlacementTestHarness) {
				home := harness.root.children["home"]
				home.mode = uint16(unix.S_IFDIR | 0o730)
				home.acl = linuxTestAccessACL(
					linuxTestACLEntry{linuxPOSIXACLUserObject, 0o7, linuxPOSIXACLUndefinedID},
					linuxTestACLEntry{linuxPOSIXACLNamedUser, 0o3, 2000},
					linuxTestACLEntry{linuxPOSIXACLGroupObject, 0, linuxPOSIXACLUndefinedID},
					linuxTestACLEntry{linuxPOSIXACLMask, 0o3, linuxPOSIXACLUndefinedID},
					linuxTestACLEntry{linuxPOSIXACLOther, 0, linuxPOSIXACLUndefinedID},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness, expected := newLinuxPlacementTestHarness()
			test.configure(harness)
			system := harness.system()
			_, err := linuxCertifyAbsoluteOutputPlacement("/home/receiver/output", &system, expected)
			assertLinuxUnsafe(t, err)
		})
	}
}

func TestLinuxAbsolutePlacementRejectsUncertifiedMountAndReplacementGap(t *testing.T) {
	t.Run("non-ext4 ancestor", func(t *testing.T) {
		harness, expected := newLinuxPlacementTestHarness()
		harness.root.children["home"].magic = unix.TMPFS_MAGIC
		system := harness.system()
		_, err := linuxCertifyAbsoluteOutputPlacement("/home/receiver/output", &system, expected)
		assertLinuxUnsupported(t, err)
	})

	t.Run("component replacement", func(t *testing.T) {
		harness, expected := newLinuxPlacementTestHarness()
		harness.replaceAfterOpen = true
		system := harness.system()
		_, err := linuxCertifyAbsoluteOutputPlacement("/home/receiver/output", &system, expected)
		assertLinuxUnsafe(t, err)
	})
}

func TestLinuxAnchoredDirectoryClaimFramesPlacementAndObject(t *testing.T) {
	placement := []byte("placement")
	object := []byte("object")
	claim, err := linuxEncodeAnchoredDirectoryClaim(placement, object)
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	domain := decodeLinuxLength16(t, claim, &offset)
	if string(domain) != linuxAnchoredDirectoryClaimDomain {
		t.Fatalf("claim domain = %q", domain)
	}
	if got := decodeLinuxUint32(t, claim, &offset); got != uint32(len(placement)) {
		t.Fatalf("placement preimage length = %d, want %d", got, len(placement))
	}
	digest := decodeLinuxBytes(t, claim, &offset, sha256.Size)
	wantDigest := sha256.Sum256(placement)
	if !bytes.Equal(digest, wantDigest[:]) {
		t.Fatalf("placement digest = %x, want %x", digest, wantDigest)
	}
	if got := decodeLinuxLength32(t, claim, &offset); !bytes.Equal(got, object) {
		t.Fatalf("object field = %q", got)
	}
	if offset != len(claim) {
		t.Fatalf("claim has %d trailing bytes", len(claim)-offset)
	}
	if len(claim) > resumestate.MaxAncestryIdentityClaimBytes {
		t.Fatalf("anchored claim length = %d, maximum %d", len(claim), resumestate.MaxAncestryIdentityClaimBytes)
	}
}

func TestLinuxPlacementBoundsPathComponentsAndPreimage(t *testing.T) {
	harness, expected := newLinuxPlacementTestHarness()
	system := harness.system()
	tooLongPath := "/" + strings.Repeat("a", linuxMaximumAbsolutePathBytes)
	if _, err := linuxCertifyAbsoluteOutputPlacement(tooLongPath, &system, expected); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("oversized absolute path error = %v", err)
	}

	record := linuxOutputPlacementRecord{
		component: strings.Repeat("a", linuxOutputNameMaximumBytes),
		directory: linuxPlacementRestartIdentity(101, 11, 11),
	}
	records := make([]linuxOutputPlacementRecord, linuxMaximumPlacementComponents+1)
	for index := range records {
		records[index] = record
		records[index].directory.inode += uint64(index)
	}
	if _, err := linuxEncodeAbsolutePlacementClaim(records); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("oversized placement preimage error = %v", err)
	}

	records = append(records, record)
	if _, err := linuxEncodeAbsolutePlacementClaim(records); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("oversized placement record count error = %v", err)
	}
}

func TestLinuxPlacementDigestBindsPathOrderMountAndIncarnation(t *testing.T) {
	record := func(component string, inode uint64, generation uint32) linuxOutputPlacementRecord {
		return linuxOutputPlacementRecord{
			component: component,
			directory: linuxPlacementRestartIdentity(inode, generation, generation),
		}
	}
	baseRecords := []linuxOutputPlacementRecord{
		record("", 101, 11),
		record("home", 102, 12),
	}
	anchored := func(records []linuxOutputPlacementRecord) []byte {
		t.Helper()
		placement, err := linuxEncodeAbsolutePlacementClaim(records)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := linuxEncodeAnchoredDirectoryClaim(placement, []byte("fixed-output-object"))
		if err != nil {
			t.Fatal(err)
		}
		return claim
	}
	base := anchored(baseRecords)
	mutations := map[string]func([]linuxOutputPlacementRecord){
		"path": func(records []linuxOutputPlacementRecord) {
			records[1].component = "users"
		},
		"order": func(records []linuxOutputPlacementRecord) {
			records[0], records[1] = records[1], records[0]
		},
		"mount": func(records []linuxOutputPlacementRecord) {
			records[0].directory.mount.uniqueMountID++
		},
		"inode": func(records []linuxOutputPlacementRecord) {
			records[0].directory.inode++
		},
		"generation": func(records []linuxOutputPlacementRecord) {
			records[0].directory.generation++
		},
		"birth time": func(records []linuxOutputPlacementRecord) {
			records[0].directory.birthNanoseconds++
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := append([]linuxOutputPlacementRecord(nil), baseRecords...)
			mutate(changed)
			if bytes.Equal(base, anchored(changed)) {
				t.Fatalf("%s change did not alter anchored placement claim", name)
			}
		})
	}
}

func linuxPlacementRestartIdentity(
	inode uint64,
	birthNanoseconds uint32,
	generation uint32,
) linuxDirectoryRestartIdentity {
	return linuxDirectoryRestartIdentity{
		mount: linuxMountIdentity{
			uniqueMountID:       linuxPlacementTestUniqueMount,
			deviceMajor:         8,
			deviceMinor:         1,
			runtimeFilesystemID: [2]int32{23, 29},
			filesystemUUID:      linuxTestFilesystemUUID,
		},
		inode: inode, kind: unix.S_IFDIR,
		birthSeconds: 1_700_000_000, birthNanoseconds: birthNanoseconds,
		generation: generation, hasGenerationProof: generation != 0,
	}
}

func newLinuxPlacementTestHarness() (*linuxPlacementTestHarness, linuxOutputCertificate) {
	node := func(fd int, inode uint64, generation uint32, owner uint32, mode uint16) *linuxPlacementTestNode {
		return &linuxPlacementTestNode{
			fd: fd, inode: inode, generation: generation, ownerUID: owner,
			birthNanoseconds: generation,
			mode:             uint16(unix.S_IFDIR) | mode, magic: linuxExt4SuperMagic,
			children: make(map[string]*linuxPlacementTestNode),
		}
	}
	root := node(10, 101, 11, 0, 0o755)
	home := node(11, 102, 12, 0, 0o755)
	receiver := node(12, 103, 13, linuxPlacementTestReceiverUID, 0o700)
	output := node(13, 104, 14, linuxPlacementTestReceiverUID, 0o700)
	root.children["home"] = home
	home.children["receiver"] = receiver
	receiver.children["output"] = output
	harness := &linuxPlacementTestHarness{
		nodes: map[int]*linuxPlacementTestNode{
			root.fd: root, home.fd: home, receiver.fd: receiver, output.fd: output,
		},
		root: root, namedObservations: make(map[string]int),
	}
	return harness, harness.certificate(output)
}

func (harness *linuxPlacementTestHarness) certificate(node *linuxPlacementTestNode) linuxOutputCertificate {
	restart := linuxPlacementRestartIdentity(node.inode, node.birthNanoseconds, node.generation)
	return linuxOutputCertificate{
		mount: restart.mount,
		rootObject: linuxOpenHandleIdentity{
			mountID:     linuxPlacementTestUniqueMount,
			deviceMajor: 8, deviceMinor: 1,
			inode: node.inode, kind: unix.S_IFDIR,
		},
		rootRestartIdentity: restart,
		durability:          linuxOutputProcessRestartDurability,
	}
}

func (harness *linuxPlacementTestHarness) system() linuxOutputSystem {
	return linuxOutputSystem{
		openat2: func(dirfd int, path string, _ *unix.OpenHow) (int, error) {
			if dirfd == unix.AT_FDCWD && path == "/" {
				return harness.root.fd, nil
			}
			parent := harness.nodes[dirfd]
			if parent == nil {
				return -1, unix.EBADF
			}
			child := parent.children[path]
			if child == nil {
				return -1, unix.ENOENT
			}
			return child.fd, nil
		},
		close: func(int) error { return nil },
		statx: func(fd int, path string, _ int, mask int, stat *unix.Statx_t) error {
			node := harness.nodes[fd]
			if node == nil {
				return unix.EBADF
			}
			if path != "" {
				child := node.children[path]
				if child == nil {
					return unix.ENOENT
				}
				node = child
				key := fmt.Sprintf("%d/%s", fd, path)
				harness.namedObservations[key]++
				if harness.replaceAfterOpen && path == "output" && harness.namedObservations[key] > 1 {
					clone := *node
					clone.inode++
					node = &clone
				}
			}
			returnedMask := uint32(mask)
			mountID := linuxPlacementTestMountID
			if mask&unix.STATX_MNT_ID_UNIQUE != 0 {
				mountID = linuxPlacementTestUniqueMount
			}
			*stat = unix.Statx_t{
				Mask: returnedMask,
				Ino:  node.inode, Mode: node.mode, Uid: node.ownerUID,
				Dev_major: 8, Dev_minor: 1, Mnt_id: mountID,
				Btime: unix.StatxTimestamp{
					Sec: 1_700_000_000, Nsec: node.birthNanoseconds,
				},
			}
			return nil
		},
		fstatfs: func(fd int, stat *unix.Statfs_t) error {
			node := harness.nodes[fd]
			if node == nil {
				return unix.EBADF
			}
			reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(node.magic)
			stat.Fsid.Val = [2]int32{23, 29}
			return nil
		},
		faccessat2: func(int, string, uint32, int) error { return nil },
		fgetxattr: func(fd int, name string, destination []byte) (int, error) {
			if name != linuxAccessACL {
				return 0, unix.ENODATA
			}
			node := harness.nodes[fd]
			if node == nil {
				return 0, unix.EBADF
			}
			if len(node.acl) == 0 {
				return 0, unix.ENODATA
			}
			if destination == nil {
				return len(node.acl), nil
			}
			return copy(destination, node.acl), nil
		},
		geteuid: func() int { return int(linuxPlacementTestReceiverUID) },
		getVersion: func(fd int) (uint32, error) {
			node := harness.nodes[fd]
			if node == nil {
				return 0, unix.EBADF
			}
			return node.generation, nil
		},
		getFlags: func(int) (uint32, error) { return 0, nil },
		getFilesystemUUID: func(int) ([linuxFilesystemUUIDBytes]byte, error) {
			return linuxTestFilesystemUUID, nil
		},
		restartIdentity:   linuxStatxBirthTimeRestartIdentityProvider{},
		readProcessStatus: func() ([]byte, error) { return []byte("Umask:\t0022\n"), nil },
		readMountInfo: func() ([]byte, error) {
			return fmt.Appendf(nil,
				"%d 1 8:1 / / rw - ext4 /dev/test rw\n",
				linuxPlacementTestMountID,
			), nil
		},
	}
}

func decodeLinuxPlacementClaim(t *testing.T, claim []byte) ([]string, []uint32, []uint32) {
	t.Helper()
	offset := 0
	domain := decodeLinuxLength16(t, claim, &offset)
	if string(domain) != linuxAbsolutePlacementClaimDomain {
		t.Fatalf("placement domain = %q", domain)
	}
	count := int(decodeLinuxUint32(t, claim, &offset))
	components := make([]string, 0, count)
	birthNanoseconds := make([]uint32, 0, count)
	generations := make([]uint32, 0, count)
	for range count {
		record := decodeLinuxLength32(t, claim, &offset)
		recordOffset := 0
		components = append(components, string(decodeLinuxLength16(t, record, &recordOffset)))
		identity := decodeLinuxLength32(t, record, &recordOffset)
		identityOffset := 0
		if domain := string(decodeLinuxLength16(t, identity, &identityOffset)); domain != linuxDirectoryRestartIdentityClaimDomain {
			t.Fatalf("directory identity domain = %q", domain)
		}
		mount := decodeLinuxLength32(t, identity, &identityOffset)
		mountOffset := 0
		if domain := string(decodeLinuxLength16(t, mount, &mountOffset)); domain != linuxMountIdentityClaimDomain {
			t.Fatalf("mount identity domain = %q", domain)
		}
		const mountFieldsBytes = 8 + 4 + 4 + 4 + 4 + linuxFilesystemUUIDBytes
		mountOffset += mountFieldsBytes
		if mountOffset != len(mount) {
			t.Fatalf("mount identity has %d trailing bytes", len(mount)-mountOffset)
		}
		_ = decodeLinuxUint64(t, identity, &identityOffset)
		_ = decodeLinuxUint64(t, identity, &identityOffset)
		birthNanoseconds = append(birthNanoseconds, decodeLinuxUint32(t, identity, &identityOffset))
		hasGeneration := decodeLinuxBytes(t, identity, &identityOffset, 1)[0]
		generation := decodeLinuxUint32(t, identity, &identityOffset)
		if hasGeneration != 1 {
			t.Fatalf("placement generation presence = %d", hasGeneration)
		}
		generations = append(generations, generation)
		if got := decodeLinuxUint16(t, identity, &identityOffset); got != unix.S_IFDIR {
			t.Fatalf("placement file type = %#x", got)
		}
		if identityOffset != len(identity) {
			t.Fatalf("directory identity has %d trailing bytes", len(identity)-identityOffset)
		}
		if recordOffset != len(record) {
			t.Fatalf("placement record has %d trailing bytes", len(record)-recordOffset)
		}
	}
	if offset != len(claim) {
		t.Fatalf("placement claim has %d trailing bytes", len(claim)-offset)
	}
	return components, birthNanoseconds, generations
}

func decodeLinuxLength16(t *testing.T, encoded []byte, offset *int) []byte {
	t.Helper()
	length := int(decodeLinuxUint16(t, encoded, offset))
	return decodeLinuxBytes(t, encoded, offset, length)
}

func decodeLinuxLength32(t *testing.T, encoded []byte, offset *int) []byte {
	t.Helper()
	length := int(decodeLinuxUint32(t, encoded, offset))
	return decodeLinuxBytes(t, encoded, offset, length)
}

func decodeLinuxBytes(t *testing.T, encoded []byte, offset *int, length int) []byte {
	t.Helper()
	if length < 0 || *offset < 0 || length > len(encoded)-*offset {
		t.Fatalf("decode length %d at offset %d exceeds %d bytes", length, *offset, len(encoded))
	}
	value := encoded[*offset : *offset+length]
	*offset += length
	return value
}

func decodeLinuxUint16(t *testing.T, encoded []byte, offset *int) uint16 {
	t.Helper()
	value := decodeLinuxBytes(t, encoded, offset, 2)
	return binary.BigEndian.Uint16(value)
}

func decodeLinuxUint32(t *testing.T, encoded []byte, offset *int) uint32 {
	t.Helper()
	value := decodeLinuxBytes(t, encoded, offset, 4)
	return binary.BigEndian.Uint32(value)
}

func decodeLinuxUint64(t *testing.T, encoded []byte, offset *int) uint64 {
	t.Helper()
	value := decodeLinuxBytes(t, encoded, offset, 8)
	return binary.BigEndian.Uint64(value)
}

func TestLinuxPlacementHarnessDetectsExpectedObjectMismatch(t *testing.T) {
	harness, expected := newLinuxPlacementTestHarness()
	expected.rootObject.inode++
	system := harness.system()
	_, err := linuxCertifyAbsoluteOutputPlacement("/home/receiver/output", &system, expected)
	if !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("mismatched final object error = %v", err)
	}
}
