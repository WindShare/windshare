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
	aclReferenceBytes  = 8
	aclHeaderBytes     = 44
	aclEntryBytes      = 24
	aclMagic           = 0x012cc16d
	aclNoEntries       = ^uint32(0)
	aclKindMask        = 0xf
	aclDeny            = 2
	aclCountOffset     = 36
	aclFlagsOffset     = 40
	aclNativeFlags     = 0xffff
	aclNoInherit       = 1 << 17
	attributeModeBytes = 4
)

// Mode 0700 does not revoke ACL grants. Private state must also reject
// inheritable grants, because future checkpoints contain verified plaintext.
func validatePrivateACL(fd int) error {
	security, err := readSecurity(fd)
	if err != nil || len(security) == 0 {
		return err
	}
	for entry := security[aclHeaderBytes:]; len(entry) != 0; entry = entry[aclEntryBytes:] {
		if binary.LittleEndian.Uint32(entry[16:])&aclKindMask != aclDeny {
			return outputcap.ErrUnsafeNamespace
		}
	}
	return nil
}

// The packed format follows Apple's kauth_filesec and attrreference ABI.
// Keeping raw security separate from its policy also permits initialization of
// a newly created private directory beneath an ordinary inheriting container.
func readSecurity(fd int) ([]byte, error) {
	request := unix.Attrlist{Bitmapcount: attributeBitmapCount, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, attributeBufferBytes)
	_, _, errno := libSystemCall6(fgetattrlistAddress, uintptr(fd), uintptr(unsafe.Pointer(&request)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(&request)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return nil, nativeError(errno)
	}
	length := int(binary.LittleEndian.Uint32(buffer))
	if length < 4+aclReferenceBytes || length > len(buffer) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	reference := buffer[4:length]
	size := int(binary.LittleEndian.Uint32(reference[4:]))
	if size == 0 {
		return nil, nil
	}
	offset := int(int32(binary.LittleEndian.Uint32(reference)))
	if offset < aclReferenceBytes || offset > len(reference) || size > len(reference)-offset || size < aclHeaderBytes {
		return nil, outputcap.ErrUnsafeNamespace
	}
	security := reference[offset : offset+size]
	if binary.LittleEndian.Uint32(security) != aclMagic {
		return nil, outputcap.ErrUnsafeNamespace
	}
	count := binary.LittleEndian.Uint32(security[aclCountOffset:])
	if count == aclNoEntries {
		if size != aclHeaderBytes {
			return nil, outputcap.ErrUnsafeNamespace
		}
		return security, nil
	}
	if uint64(count)*aclEntryBytes+aclHeaderBytes != uint64(size) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return security, nil
}

func setPrivateDirectorySecurity(fd int) error {
	inherited, err := readSecurity(fd)
	if err != nil {
		return err
	}
	flags := uint32(aclNoInherit)
	if len(inherited) != 0 {
		flags |= binary.LittleEndian.Uint32(inherited[aclFlagsOffset:]) & aclNativeFlags
	}
	// Set mode and an empty, non-inheriting ACL on the retained empty object
	// before admitting any private children. The parent keeps its own ACL.
	security := make([]byte, aclHeaderBytes)
	binary.LittleEndian.PutUint32(security, aclMagic)
	binary.LittleEndian.PutUint32(security[aclFlagsOffset:], flags)
	return setSecurityAttributes(fd, privateDirectoryMode, nil, security)
}

func setSecurityAttributes(fd int, mode uint32, group *uint32, security []byte) error {
	if len(security) == 0 {
		security = make([]byte, aclHeaderBytes)
		binary.LittleEndian.PutUint32(security, aclMagic)
		binary.LittleEndian.PutUint32(security[aclCountOffset:], aclNoEntries)
	}
	prefix := attributeModeBytes
	common := uint32(unix.ATTR_CMN_ACCESSMASK | unix.ATTR_CMN_EXTENDED_SECURITY)
	if group != nil {
		prefix += attributeModeBytes
		common |= unix.ATTR_CMN_GRPID
	}
	buffer := make([]byte, prefix+aclReferenceBytes+len(security))
	if group != nil {
		binary.LittleEndian.PutUint32(buffer, *group)
	}
	binary.LittleEndian.PutUint32(buffer[prefix-attributeModeBytes:], mode)
	reference := buffer[prefix:]
	binary.LittleEndian.PutUint32(reference, aclReferenceBytes)
	binary.LittleEndian.PutUint32(reference[4:], uint32(len(security)))
	copy(reference[aclReferenceBytes:], security)
	request := unix.Attrlist{Bitmapcount: attributeBitmapCount, Commonattr: common}
	_, _, errno := libSystemCall6(fsetattrlistAddress, uintptr(fd), uintptr(unsafe.Pointer(&request)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(&request)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return nativeError(errno)
	}
	return nil
}
