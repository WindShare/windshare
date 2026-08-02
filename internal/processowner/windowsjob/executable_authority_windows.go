//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"unsafe"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

const maximumOwnedExecutableBytes = 512 << 20

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

type windowsExecutableAuthority struct {
	file     *os.File
	identity ownerprotocol.ObjectIdentity
}

type windowsFileIDInfo struct {
	volume uint64
	object [16]byte
}

func resumeContainedTarget(handle windows.Handle) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return errors.New("contained target process is unavailable")
	}
	status, _, _ := ntResumeProcess.Call(uintptr(handle))
	if int32(status) < 0 {
		return fmt.Errorf("resume contained target: NTSTATUS 0x%08x", uint32(status))
	}
	return nil
}

func holdWindowsExecutable(path string) (*windowsExecutableAuthority, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("retain owned executable: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("adopt owned executable handle")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximumOwnedExecutableBytes {
		return nil, errors.Join(errors.New("owned executable is not a bounded regular file"), err, file.Close())
	}
	identity, attributes, err := windowsObjectIdentity(handle)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, errors.Join(errors.New("owned executable is a reparse point or directory"), err, file.Close())
	}
	return &windowsExecutableAuthority{file: file, identity: identity}, nil
}

func (authority *windowsExecutableAuthority) close() error {
	if authority == nil || authority.file == nil {
		return nil
	}
	err := authority.file.Close()
	authority.file = nil
	return err
}

func (authority *windowsExecutableAuthority) startEvidence(
	identity ownerprotocol.Identity,
	processID uint32,
	process windows.Handle,
) (ownerprotocol.StartEvidence, error) {
	if authority == nil || authority.file == nil {
		return ownerprotocol.StartEvidence{}, errors.New("owned executable authority is unavailable")
	}
	actualPID, err := windows.GetProcessId(process)
	if err != nil || actualPID != processID {
		return ownerprotocol.StartEvidence{}, errors.Join(
			errors.New("retained target PID does not match its launch evidence"),
			err,
		)
	}
	imageIdentity, err := windowsProcessImageIdentity(process)
	if err != nil {
		return ownerprotocol.StartEvidence{}, err
	}
	if imageIdentity != authority.identity {
		return ownerprotocol.StartEvidence{}, errors.New("suspended target image differs from the retained executable authority")
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return ownerprotocol.StartEvidence{}, fmt.Errorf("identify suspended target process instance: %w", err)
	}
	instance := filetimeValue(creation)
	if instance == 0 {
		return ownerprotocol.StartEvidence{}, errors.New("suspended target process instance is unavailable")
	}
	return ownerprotocol.StartEvidence{
		SchemaVersion:   ownerprotocol.StartEvidenceSchemaVersion,
		Identity:        identity,
		Platform:        ownerprotocol.PlatformWindowsJob,
		ProcessID:       int(processID),
		ProcessInstance: strconv.FormatUint(instance, 10),
		Executable:      authority.identity,
	}, nil
}

func windowsProcessImageIdentity(process windows.Handle) (ownerprotocol.ObjectIdentity, error) {
	buffer := make([]uint16, 32_768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return ownerprotocol.ObjectIdentity{}, fmt.Errorf("identify suspended target image path: %w", err)
	}
	path := windows.UTF16ToString(buffer[:size])
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ownerprotocol.ObjectIdentity{}, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return ownerprotocol.ObjectIdentity{}, fmt.Errorf("open suspended target image identity: %w", err)
	}
	identity, attributes, identityErr := windowsObjectIdentity(handle)
	closeErr := windows.CloseHandle(handle)
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		identityErr = errors.Join(identityErr, errors.New("suspended target image is not a regular no-follow file"))
	}
	return identity, errors.Join(identityErr, closeErr)
}

func windowsObjectIdentity(handle windows.Handle) (ownerprotocol.ObjectIdentity, uint32, error) {
	var identity windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&identity)),
		uint32(unsafe.Sizeof(identity)),
	); err != nil {
		return ownerprotocol.ObjectIdentity{}, 0, err
	}
	var metadata windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &metadata); err != nil {
		return ownerprotocol.ObjectIdentity{}, 0, err
	}
	return ownerprotocol.NewObjectIdentity128(identity.volume, identity.object), metadata.FileAttributes, nil
}

func filetimeValue(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}
