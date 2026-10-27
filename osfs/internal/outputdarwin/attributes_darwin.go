//go:build darwin

package outputdarwin

import (
	"encoding/binary"
	"errors"
	"runtime"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

const (
	attributeBitmapCount  = 5
	attributeHeaderBytes  = 24
	attributeBufferBytes  = 4096
	volumeUUIDBytes       = 16
	volumeCapabilityBytes = 32
	volumePersistentIDs   = 0x1
	volumeCaseSensitive   = 0x100
	volumeNonrecycledIDs  = 0x4000
	volume64BitIDs        = 0x20000
	nanosecondsPerSecond  = 1_000_000_000
)

// Darwin packs attributes at four-byte boundaries, including 64-bit fields.
// Decode bytes explicitly instead of depending on Go struct alignment.
func attributes(fd int, common, volume uint32) ([]byte, error) {
	request := unix.Attrlist{Bitmapcount: attributeBitmapCount,
		Commonattr: common | unix.ATTR_CMN_RETURNED_ATTRS, Volattr: volume}
	buffer := make([]byte, attributeBufferBytes)
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&request)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(&request)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return nil, nativeError(errno)
	}
	size := int(binary.LittleEndian.Uint32(buffer))
	if size < attributeHeaderBytes || size > len(buffer) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	returnedCommon := binary.LittleEndian.Uint32(buffer[4:])
	returnedVolume := binary.LittleEndian.Uint32(buffer[8:])
	wantedVolume := volume &^ uint32(unix.ATTR_VOL_INFO)
	if returnedCommon&common != common || returnedVolume&wantedVolume != wantedVolume {
		return nil, outputcap.ErrRecoverableOutputUnsupported
	}
	return buffer[attributeHeaderBytes:size], nil
}

type volumeEvidence struct {
	uuid          [volumeUUIDBytes]byte
	filesystem    unix.Fsid
	device        int32
	restart       bool
	caseSensitive bool
}

func inspectVolume(fd int) (volumeEvidence, error) {
	var stat unix.Statfs_t
	var object unix.Stat_t
	if err := errors.Join(unix.Fstatfs(fd, &stat), unix.Fstat(fd, &object)); err != nil {
		return volumeEvidence{}, nativeError(err)
	}
	evidence := volumeEvidence{filesystem: stat.Fsid, device: object.Dev}
	caps, err := attributes(fd, 0, unix.ATTR_VOL_INFO|unix.ATTR_VOL_CAPABILITIES)
	if err != nil && !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		return evidence, err
	}
	if len(caps) == volumeCapabilityBytes {
		format := binary.LittleEndian.Uint32(caps)
		valid := binary.LittleEndian.Uint32(caps[16:])
		evidence.caseSensitive = format&valid&volumeCaseSensitive != 0
		const identityBits = volumePersistentIDs | volumeNonrecycledIDs | volume64BitIDs
		evidence.restart = format&valid&identityBits == identityBits
	}
	uuid, err := attributes(fd, 0, unix.ATTR_VOL_INFO|unix.ATTR_VOL_UUID)
	if err != nil && !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		return evidence, err
	}
	if len(uuid) == volumeUUIDBytes {
		copy(evidence.uuid[:], uuid)
	}
	// This evidence contract is backed by APFS process-restart semantics. Other
	// filesystems can still prove ordinary publication through live probes.
	evidence.restart = evidence.restart && unix.ByteSliceToString(stat.Fstypename[:]) == "apfs" &&
		stat.Flags&unix.MNT_LOCAL != 0 && evidence.uuid != [volumeUUIDBytes]byte{}
	return evidence, nil
}

func nativeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.ENOSYS):
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, err)
	case errors.Is(err, unix.ELOOP):
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	default:
		return err
	}
}

func validateModifiedTime(modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	_, err := catalog.NewModifiedTime(modified.Seconds(), modified.Nanoseconds(), modified.Precision())
	return err
}

func setModifiedTime(fd int, modified catalog.ModifiedTime) error {
	if err := validateModifiedTime(modified); err != nil || !modified.Present() {
		return err
	}
	request := unix.Attrlist{Bitmapcount: attributeBitmapCount, Commonattr: unix.ATTR_CMN_MODTIME}
	value := [2]int64{modified.Seconds(), int64(modified.Nanoseconds())}
	_, _, errno := unix.Syscall6(unix.SYS_FSETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&request)),
		uintptr(unsafe.Pointer(&value)), unsafe.Sizeof(value), 0, 0)
	runtime.KeepAlive(&request)
	runtime.KeepAlive(&value)
	if errno != 0 {
		return nativeError(errno)
	}
	return nil
}
