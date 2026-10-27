//go:build darwin

package outputdarwin

import (
	"encoding/binary"
	"runtime"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

const (
	aclReferenceBytes = 8
	aclHeaderBytes    = 44
	aclEntryBytes     = 24
	aclMagic          = 0x012cc16d
	aclNoEntries      = ^uint32(0)
	aclKindMask       = 0xf
	aclPermit         = 1
	aclDeny           = 2
	aclWriteRights    = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23
)

// Darwin ACL grants are independent of mode bits. Do not treat 0700 as private
// when an ACL can grant access to another principal. The packed format follows
// Apple's bsd/sys/kauth.h and bsd/vfs/vfs_attrlist.c.
func validateACL(fd int, private bool) error {
	request := unix.Attrlist{Bitmapcount: attributeBitmapCount, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, attributeBufferBytes)
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&request)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(&request)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return nativeError(errno)
	}
	length := int(binary.LittleEndian.Uint32(buffer))
	if length < 4+aclReferenceBytes || length > len(buffer) {
		return outputcap.ErrUnsafeNamespace
	}
	reference := buffer[4:length]
	size := int(binary.LittleEndian.Uint32(reference[4:]))
	if size == 0 {
		return nil
	}
	offset := int(int32(binary.LittleEndian.Uint32(reference)))
	if offset < aclReferenceBytes || offset > len(reference) || size > len(reference)-offset || size < aclHeaderBytes {
		return outputcap.ErrUnsafeNamespace
	}
	security := reference[offset : offset+size]
	if binary.LittleEndian.Uint32(security) != aclMagic {
		return outputcap.ErrUnsafeNamespace
	}
	count := binary.LittleEndian.Uint32(security[36:])
	if count == aclNoEntries {
		return nil
	}
	if uint64(count)*aclEntryBytes+aclHeaderBytes != uint64(size) {
		return outputcap.ErrUnsafeNamespace
	}
	for entry := security[aclHeaderBytes:]; len(entry) != 0; entry = entry[aclEntryBytes:] {
		kind := binary.LittleEndian.Uint32(entry[16:]) & aclKindMask
		rights := binary.LittleEndian.Uint32(entry[20:])
		// Denials cannot expand authority. Inherited grants also matter because
		// descendants will own private checkpoints and verified plaintext.
		if kind != aclDeny && (kind != aclPermit || private || rights&aclWriteRights != 0) {
			return outputcap.ErrUnsafeNamespace
		}
	}
	return nil
}
