//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsVolumeFacts struct {
	filesystem string
	path       string
	driveType  uint32
	flags      uint32
}

type windowsVolumeInspector interface {
	InspectVolume(windows.Handle) (windowsVolumeFacts, error)
}

type nativeWindowsVolumeInspector struct{}

func (nativeWindowsVolumeInspector) InspectVolume(handle windows.Handle) (windowsVolumeFacts, error) {
	var filesystem [32]uint16
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(
		handle, nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem)),
	); err != nil {
		return windowsVolumeFacts{}, err
	}
	path, err := windowsFinalPath(handle, 0)
	if err != nil {
		return windowsVolumeFacts{}, err
	}
	volumePath, err := windowsVolumePath(path)
	if err != nil {
		return windowsVolumeFacts{}, err
	}
	return windowsVolumeFacts{
		filesystem: windows.UTF16ToString(filesystem[:]),
		path:       path,
		driveType:  windows.GetDriveType(&volumePath[0]),
		flags:      flags,
	}, nil
}

// Object evidence is handle-bound. Names are separate placement evidence and
// must be queried only at namespace boundaries, never as an identity side effect.
// Mutable attributes are still observed afresh; no path or attribute cache grants
// authority to a later operation.
type windowsObjectFacts struct {
	attributes    uint32
	caseSensitive bool
	object        windowsObjectIdentity
}

type windowsObjectInspector interface {
	Identify(windows.Handle) (windowsObjectIdentity, error)
	Inspect(windows.Handle) (windowsObjectFacts, error)
}

type nativeWindowsObjectInspector struct{}

type windowsFileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func (nativeWindowsObjectInspector) Identify(handle windows.Handle) (windowsObjectIdentity, error) {
	// Resolve the actual volume GUID as well as the serial from this handle.
	// An expected volume or a drive-letter cache is not evidence of membership.
	volumeGUIDPath, err := windowsFinalPath(handle, windowsOutputVolumeNameGUID)
	if err != nil {
		return windowsObjectIdentity{}, err
	}
	volumeGUID := strings.ToLower(filepath.VolumeName(volumeGUIDPath))
	if volumeGUID == "" {
		return windowsObjectIdentity{}, errors.New("windows volume GUID path has no volume name")
	}

	var fileID windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return windowsObjectIdentity{}, err
	}
	return windowsObjectIdentity{
		volume: windowsVolumeIdentity{guid: volumeGUID, serial: fileID.VolumeSerialNumber},
		fileID: fileID.FileID,
	}, nil
}

func (inspector nativeWindowsObjectInspector) Inspect(handle windows.Handle) (windowsObjectFacts, error) {
	identity, err := inspector.Identify(handle)
	if err != nil {
		return windowsObjectFacts{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsObjectFacts{}, err
	}
	caseSensitive := false
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		var sensitivity struct{ Flags uint32 }
		if err := windows.GetFileInformationByHandleEx(
			handle,
			windows.FileCaseSensitiveInfo,
			(*byte)(unsafe.Pointer(&sensitivity)),
			uint32(unsafe.Sizeof(sensitivity)),
		); err != nil {
			return windowsObjectFacts{}, err
		}
		caseSensitive = sensitivity.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0
	}
	return windowsObjectFacts{
		attributes:    information.FileAttributes,
		caseSensitive: caseSensitive,
		object:        identity,
	}, nil
}

func windowsFinalPath(handle windows.Handle, flags uint32) (string, error) {
	buffer := make([]uint16, 512)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), flags)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func windowsVolumePath(path string) ([]uint16, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	buffer := make([]uint16, 512)
	if err := windows.GetVolumePathName(encoded, &buffer[0], uint32(len(buffer))); err != nil {
		return nil, err
	}
	return buffer, nil
}

func validateWindowsRootShape(facts windowsObjectFacts) error {
	if err := validateWindowsDirectoryShape(facts, "certify output root", "output root"); err != nil {
		return err
	}
	if facts.caseSensitive {
		return windowsFailure("certify output root", "", errWindowsOutputUnsupported,
			errors.New("case-sensitive NTFS output directories are not certified"))
	}
	return nil
}

func validateWindowsExternalPlacement(
	facts windowsObjectFacts,
	expected windowsVolumeIdentity,
) error {
	// The retained root already certified the volume. Each ancestry handle must
	// independently prove that same GUID and serial before reusing its capabilities.
	if facts.object.volume != expected {
		return windowsFailure("certify external output placement", "", errWindowsOutputUnsafe,
			errors.New("external placement crossed the certified NTFS volume boundary"))
	}
	// External components are spelling/placement authorities, not output lookup
	// roots. Per-directory case-sensitive lookup therefore does not affect the
	// handle-bound output namespace beneath them.
	return validateWindowsDirectoryShape(
		facts, "certify external output placement", "external output placement",
	)
}

func validateWindowsVolumeCertification(facts windowsVolumeFacts) error {
	switch {
	case !strings.EqualFold(facts.filesystem, windowsOutputFilesystem):
		return windowsFailure("certify output filesystem", facts.path, errWindowsOutputUnsupported,
			fmt.Errorf("filesystem %q is not NTFS", facts.filesystem))
	case strings.HasPrefix(strings.TrimPrefix(facts.path, `\\?\`), `UNC\`):
		return windowsFailure("certify output filesystem", facts.path, errWindowsOutputUnsupported,
			errors.New("network filesystems are not certified"))
	case facts.driveType != windows.DRIVE_FIXED:
		return windowsFailure("certify output filesystem", facts.path, errWindowsOutputUnsupported,
			fmt.Errorf("drive type %d is not a certified fixed disk", facts.driveType))
	case facts.flags&windows.FILE_SUPPORTS_HARD_LINKS == 0:
		return windowsFailure("certify output filesystem", facts.path, errWindowsOutputUnsupported,
			errors.New("volume does not report regular-file hard-link support"))
	case facts.flags&windows.FILE_PERSISTENT_ACLS == 0:
		return windowsFailure("certify output filesystem", facts.path, errWindowsOutputUnsupported,
			errors.New("volume does not report persistent ACL support"))
	case facts.flags&windowsFileSupportsPOSIXSemantics == 0:
		return windowsFailure("certify output filesystem", facts.path, errWindowsOutputUnsupported,
			errors.New("volume does not report handle-bound rename/unlink support"))
	default:
		return nil
	}
}

func validateWindowsDirectoryShape(
	facts windowsObjectFacts,
	operation string,
	role string,
) error {
	switch {
	case facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New(role+" is not a directory"))
	case facts.attributes&windowsCloudAttributeMask != 0:
		return windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New("reparse, offline, and cloud-placeholder directories are not certified"))
	case !facts.object.valid():
		return windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New("volume or File ID identity is unavailable"))
	default:
		return nil
	}
}
