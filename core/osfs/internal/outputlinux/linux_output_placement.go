//go:build linux

package outputlinux

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"golang.org/x/sys/unix"
)

// Linux rejects an absolute pathname whose terminating NUL would exceed
// PATH_MAX before any component-wise, handle-relative certification can begin.
const linuxMaximumAbsolutePathBytes = 4096

const (
	linuxAbsolutePlacementClaimDomain     = "linux/ext4/absolute-placement/v2"
	linuxAnchoredDirectoryClaimDomain     = "linux/ext4/anchored-directory-sha256/v2"
	linuxMaximumPlacementComponents       = 4096
	linuxMaximumAbsolutePlacementClaimLen = 1 << 20
	linuxClaimUint16Bytes                 = 2
	linuxClaimUint32Bytes                 = 4
	linuxClaimUint64Bytes                 = 8
)

type linuxOutputPlacementRecord struct {
	component string
	directory linuxDirectoryRestartIdentity
}

func linuxCertifyAbsoluteOutputPlacement(
	absolutePath string,
	system *linuxOutputSystem,
	expected linuxOutputCertificate,
) (_ []byte, resultErr error) {
	const operation = "certify absolute output-root placement"
	if system == nil || system.openat2 == nil || system.close == nil || system.geteuid == nil {
		return nil, linuxUnsupported(operation, "required no-follow placement providers are unavailable", nil)
	}
	if len(absolutePath) >= linuxMaximumAbsolutePathBytes {
		return nil, linuxUnsupported(operation, "output-root path exceeds Linux PATH_MAX", nil)
	}
	cleanPath := filepath.Clean(absolutePath)
	if !filepath.IsAbs(cleanPath) || cleanPath != absolutePath {
		return nil, linuxUnsafe(operation, "output-root path is not clean and absolute", nil)
	}
	components, err := linuxAbsolutePathComponents(cleanPath)
	if err != nil {
		return nil, err
	}

	rootHow := unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: uint64(unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS),
	}
	currentFD, err := system.openat2(unix.AT_FDCWD, "/", &rootHow)
	if err != nil {
		return nil, linuxClassifyOpenError(operation, err)
	}
	defer func() {
		if currentFD >= 0 {
			resultErr = errors.Join(resultErr, system.close(currentFD))
		}
	}()

	currentCertificate, err := linuxCertifyExt4OutputFD(system, currentFD)
	if err != nil {
		return nil, err
	}
	records := make([]linuxOutputPlacementRecord, 0, len(components)+1)
	records = append(records, linuxOutputPlacementRecord{
		directory: currentCertificate.rootRestartIdentity,
	})

	for _, component := range components {
		if err := linuxValidateAbsolutePlacementParent(system, currentFD, currentCertificate); err != nil {
			return nil, err
		}
		before, err := linuxStatxNamedPlacementDirectory(system, currentFD, component)
		if err != nil {
			return nil, err
		}
		childHow := unix.OpenHow{
			Flags: uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
			Resolve: uint64(
				unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
			),
		}
		childFD, err := system.openat2(currentFD, component, &childHow)
		if err != nil {
			return nil, linuxClassifyOpenError(operation, err)
		}
		childCertificate, certifyErr := linuxCertifyExt4OutputFD(system, childFD)
		if certifyErr != nil {
			return nil, errors.Join(certifyErr, system.close(childFD))
		}
		after, statErr := linuxStatxNamedPlacementDirectory(system, currentFD, component)
		if statErr != nil {
			return nil, errors.Join(statErr, system.close(childFD))
		}
		child := childCertificate.rootObject
		if !linuxNamedPlacementMatchesOpen(before, child) ||
			!linuxNamedPlacementMatchesOpen(after, child) {
			return nil, errors.Join(
				linuxUnsafe(operation, "a path component changed across its no-follow reopen", nil),
				system.close(childFD),
			)
		}

		previousFD := currentFD
		currentFD = childFD
		currentCertificate = childCertificate
		records = append(records, linuxOutputPlacementRecord{
			component: component, directory: childCertificate.rootRestartIdentity,
		})
		if err := system.close(previousFD); err != nil {
			return nil, fmt.Errorf("%s: close traversed ancestor: %w", operation, err)
		}
	}

	reopened, err := linuxVerifyOpenObject(system, currentFD, expected)
	if err != nil {
		return nil, err
	}
	if currentCertificate.mount != expected.mount ||
		!reopened.matches(expected.rootObject) ||
		!currentCertificate.rootObject.sameObject(expected.rootObject) {
		return nil, linuxUnsafe(operation,
			"filesystem-root walk did not reopen the certified output-root object", nil)
	}
	return linuxEncodeAbsolutePlacementClaim(records)
}

func linuxAbsolutePathComponents(absolutePath string) ([]string, error) {
	const operation = "certify absolute output-root placement"
	if absolutePath == string(filepath.Separator) {
		return nil, nil
	}
	trimmed := strings.TrimPrefix(absolutePath, string(filepath.Separator))
	componentCount := strings.Count(trimmed, string(filepath.Separator)) + 1
	if componentCount > linuxMaximumPlacementComponents {
		return nil, linuxUnsupported(operation, "output-root ancestry exceeds the certified component bound", nil)
	}
	components := strings.Split(trimmed, string(filepath.Separator))
	for _, component := range components {
		if err := linuxValidateComponent(operation, component); err != nil {
			return nil, err
		}
	}
	return components, nil
}

func linuxValidateAbsolutePlacementParent(
	system *linuxOutputSystem,
	fd int,
	certificate linuxOutputCertificate,
) error {
	const operation = "validate absolute output-root ancestry authority"
	if system.faccessat2 == nil || system.fgetxattr == nil || system.geteuid == nil {
		return linuxUnsupported(operation, "required owner, access, or ACL provider is unavailable", nil)
	}
	identity, err := linuxVerifyOpenObject(system, fd, certificate)
	if err != nil {
		return err
	}
	if identity.identity.kind != unix.S_IFDIR ||
		!identity.matches(certificate.rootObject) {
		return linuxUnsafe(operation, "ancestry handle is not its certified directory incarnation", nil)
	}
	receiverUID := uint32(system.geteuid())
	if identity.ownerUID != receiverUID && identity.ownerUID != 0 {
		// A foreign owner can grant mutation authority after validation even when
		// the current mode and ACL happen to be restrictive.
		return linuxUnsafe(operation,
			"an external ancestry directory is owned by another unprivileged principal", nil)
	}
	if err := system.faccessat2(fd, "", uint32(unix.X_OK), unix.AT_EMPTY_PATH|unix.AT_EACCESS); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "handle-bound effective search checks are unavailable", err)
		}
		return linuxUnsafe(operation, "receiver lacks effective search authority on an ancestry directory", err)
	}
	reason, err := linuxExternalChildMutationAuthority(system, fd, identity.mode, receiverUID, operation)
	if err != nil {
		return err
	}
	if reason != "" {
		// Sticky semantics do not prevent an outside writer from allocating
		// colliding names, so shared sticky parents remain outside certification.
		return linuxUnsafe(operation, reason, nil)
	}
	rechecked, err := linuxVerifyOpenObject(system, fd, certificate)
	if err != nil {
		return err
	}
	if !rechecked.identity.sameObject(identity.identity) {
		return linuxUnsafe(operation, "ancestry directory changed while authority was inspected", nil)
	}
	return nil
}

func linuxStatxNamedPlacementDirectory(
	system *linuxOutputSystem,
	parentFD int,
	component string,
) (linuxNamedEntrySnapshot, error) {
	const operation = "inspect absolute output-root path component"
	requested := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := system.statx(parentFD, component, unix.AT_SYMLINK_NOFOLLOW, requested, &stat); err != nil {
		return linuxNamedEntrySnapshot{}, linuxClassifyOpenError(operation, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return linuxNamedEntrySnapshot{}, linuxUnsupported(operation,
			"filesystem omitted required no-follow component identity", nil)
	}
	identity := linuxNamedEntrySnapshot{identity: linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}}
	if identity.identity.kind != unix.S_IFDIR {
		return linuxNamedEntrySnapshot{}, linuxUnsafe(operation,
			"an absolute output-root path component is not a directory", nil)
	}
	return identity, nil
}

func linuxNamedPlacementMatchesOpen(
	named linuxNamedEntrySnapshot,
	opened linuxOpenHandleIdentity,
) bool {
	return named.matches(opened)
}

func linuxEncodeAbsolutePlacementClaim(records []linuxOutputPlacementRecord) ([]byte, error) {
	const operation = "encode absolute output-root placement claim"
	if len(records) == 0 || len(records) > linuxMaximumPlacementComponents+1 {
		return nil, linuxUnsupported(operation, "placement record count is outside its certified bound", nil)
	}
	totalBytes := linuxClaimUint16Bytes + len(linuxAbsolutePlacementClaimDomain) +
		linuxClaimUint32Bytes
	recordLengths := make([]int, len(records))
	for index, record := range records {
		recordBytes, err := linuxPlacementRecordEncodedLength(record)
		if err != nil {
			return nil, err
		}
		if recordBytes > linuxMaximumAbsolutePlacementClaimLen-linuxClaimUint32Bytes ||
			totalBytes > linuxMaximumAbsolutePlacementClaimLen-linuxClaimUint32Bytes-recordBytes {
			return nil, linuxUnsupported(operation, "placement claim exceeds its certified byte bound", nil)
		}
		recordLengths[index] = recordBytes
		totalBytes += linuxClaimUint32Bytes + recordBytes
	}

	claim := make([]byte, 0, totalBytes)
	var err error
	claim, err = linuxAppendLengthPrefixed16(claim, []byte(linuxAbsolutePlacementClaimDomain))
	if err != nil {
		return nil, linuxUnsupported(operation, "placement domain is too large", err)
	}
	claim = linuxAppendUint32(claim, uint32(len(records)))
	for index, record := range records {
		encoded, err := linuxEncodePlacementRecord(record, recordLengths[index])
		if err != nil {
			return nil, err
		}
		claim = linuxAppendUint32(claim, uint32(len(encoded)))
		claim = append(claim, encoded...)
	}
	return claim, nil
}

func linuxPlacementRecordEncodedLength(record linuxOutputPlacementRecord) (int, error) {
	const operation = "encode absolute output-root placement record"
	if len(record.component) > linuxOutputNameMaximumBytes {
		return 0, linuxUnsupported(operation, "placement component exceeds the ext4 byte bound", nil)
	}
	identity, err := linuxEncodeDirectoryRestartIdentity(record.directory)
	if err != nil {
		return 0, err
	}
	return linuxClaimUint16Bytes + len(record.component) + linuxClaimUint32Bytes + len(identity), nil
}

func linuxEncodePlacementRecord(record linuxOutputPlacementRecord, encodedLength int) ([]byte, error) {
	encoded := make([]byte, 0, encodedLength)
	var err error
	encoded, err = linuxAppendLengthPrefixed16(encoded, []byte(record.component))
	if err != nil {
		return nil, linuxUnsupported(
			"encode absolute output-root placement record",
			"placement component cannot be length framed",
			err,
		)
	}
	identity, err := linuxEncodeDirectoryRestartIdentity(record.directory)
	if err != nil {
		return nil, err
	}
	encoded = linuxAppendUint32(encoded, uint32(len(identity)))
	encoded = append(encoded, identity...)
	if len(encoded) != encodedLength {
		return nil, linuxUnsafe(
			"encode absolute output-root placement record",
			"placement record length calculation diverged from its encoding",
			nil,
		)
	}
	return encoded, nil
}

func linuxEncodeAnchoredDirectoryClaim(placement, object []byte) ([]byte, error) {
	const operation = "encode anchored output directory claim"
	if len(placement) == 0 || len(object) == 0 ||
		len(placement) > linuxMaximumAbsolutePlacementClaimLen ||
		uint64(len(placement)) > uint64(math.MaxUint32) ||
		uint64(len(object)) > uint64(math.MaxUint32) {
		return nil, linuxUnsupported(operation, "claim field is empty or outside its certified bound", nil)
	}
	// The resume-state model deliberately bounds each opaque claim to 256 bytes.
	// Hashing the already domain- and length-framed placement preserves a
	// collision-resistant commitment to every ext4 incarnation without making
	// deeply nested absolute paths impossible to represent.
	placementDigest := sha256.Sum256(placement)
	claim := make(
		[]byte, 0,
		len(linuxAnchoredDirectoryClaimDomain)+linuxClaimUint16Bytes+
			2*linuxClaimUint32Bytes+sha256.Size+len(object),
	)
	var err error
	claim, err = linuxAppendLengthPrefixed16(claim, []byte(linuxAnchoredDirectoryClaimDomain))
	if err != nil {
		return nil, linuxUnsupported(operation, "claim domain is too large", err)
	}
	claim = linuxAppendUint32(claim, uint32(len(placement)))
	claim = append(claim, placementDigest[:]...)
	claim = linuxAppendUint32(claim, uint32(len(object)))
	claim = append(claim, object...)
	if len(claim) > resumestate.MaxAncestryIdentityClaimBytes {
		return nil, linuxUnsupported(operation,
			"anchored directory claim exceeds the resume-state identity bound", nil)
	}
	return claim, nil
}

func linuxAppendLengthPrefixed16(destination, value []byte) ([]byte, error) {
	if len(value) > math.MaxUint16 {
		return nil, errors.New("value exceeds uint16 length framing")
	}
	destination = linuxAppendUint16(destination, uint16(len(value)))
	return append(destination, value...), nil
}

func linuxAppendUint16(destination []byte, value uint16) []byte {
	var encoded [linuxClaimUint16Bytes]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	return append(destination, encoded[:]...)
}

func linuxAppendUint32(destination []byte, value uint32) []byte {
	var encoded [linuxClaimUint32Bytes]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return append(destination, encoded[:]...)
}

func linuxAppendUint64(destination []byte, value uint64) []byte {
	var encoded [linuxClaimUint64Bytes]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(destination, encoded[:]...)
}
