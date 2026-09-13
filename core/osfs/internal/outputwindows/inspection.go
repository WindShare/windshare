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

type windowsV3VolumeFacts struct {
	filesystem string
	path       string
	driveType  uint32
	flags      uint32
}

type windowsV3VolumeInspector interface {
	InspectVolume(windows.Handle) (windowsV3VolumeFacts, error)
}

type nativeWindowsV3VolumeInspector struct{}

func (nativeWindowsV3VolumeInspector) InspectVolume(handle windows.Handle) (windowsV3VolumeFacts, error) {
	var filesystem [32]uint16
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(
		handle, nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem)),
	); err != nil {
		return windowsV3VolumeFacts{}, err
	}
	path, err := windowsV3FinalPath(handle, 0)
	if err != nil {
		return windowsV3VolumeFacts{}, err
	}
	volumePath, err := windowsV3VolumePath(path)
	if err != nil {
		return windowsV3VolumeFacts{}, err
	}
	return windowsV3VolumeFacts{
		filesystem: windows.UTF16ToString(filesystem[:]),
		path:       path,
		driveType:  windows.GetDriveType(&volumePath[0]),
		flags:      flags,
	}, nil
}

// Object facts are read from the current handle on every inspection. Volume
// capabilities belong to root admission and cannot accidentally enter this path.
type windowsV3ObjectFacts struct {
	path          string
	attributes    uint32
	caseSensitive bool
	object        windowsV3ObjectIdentity
}

type windowsV3ObjectInspector interface {
	Inspect(windows.Handle) (windowsV3ObjectFacts, error)
}

type windowsV3ObjectInspectorFunc func(windows.Handle) (windowsV3ObjectFacts, error)

func (inspect windowsV3ObjectInspectorFunc) Inspect(handle windows.Handle) (windowsV3ObjectFacts, error) {
	return inspect(handle)
}

type nativeWindowsV3ObjectInspector struct{}

type windowsV3FileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func (nativeWindowsV3ObjectInspector) Inspect(handle windows.Handle) (windowsV3ObjectFacts, error) {
	path, err := windowsV3FinalPath(handle, 0)
	if err != nil {
		return windowsV3ObjectFacts{}, err
	}
	// Resolve the actual volume GUID as well as the serial from this handle.
	// An expected volume or a drive-letter cache is not evidence of membership.
	volumeGUIDPath, err := windowsV3FinalPath(handle, windowsV3OutputVolumeNameGUID)
	if err != nil {
		return windowsV3ObjectFacts{}, err
	}
	volumeGUID := strings.ToLower(filepath.VolumeName(volumeGUIDPath))
	if volumeGUID == "" {
		return windowsV3ObjectFacts{}, errors.New("windows volume GUID path has no volume name")
	}

	var fileID windowsV3FileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return windowsV3ObjectFacts{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsV3ObjectFacts{}, err
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
			return windowsV3ObjectFacts{}, err
		}
		caseSensitive = sensitivity.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0
	}
	return windowsV3ObjectFacts{
		path:          path,
		attributes:    information.FileAttributes,
		caseSensitive: caseSensitive,
		object: windowsV3ObjectIdentity{
			volume: windowsV3VolumeIdentity{guid: volumeGUID, serial: fileID.VolumeSerialNumber},
			fileID: fileID.FileID,
		},
	}, nil
}

func windowsV3FinalPath(handle windows.Handle, flags uint32) (string, error) {
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

func windowsV3VolumePath(path string) ([]uint16, error) {
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

func validateWindowsV3RootShape(facts windowsV3ObjectFacts) error {
	if err := validateWindowsV3DirectoryShape(facts, "certify output root", "output root"); err != nil {
		return err
	}
	if facts.caseSensitive {
		return windowsV3Failure("certify output root", facts.path, errWindowsV3OutputUnsupported,
			errors.New("case-sensitive NTFS output directories are not certified"))
	}
	return nil
}

func validateWindowsV3ExternalPlacement(
	facts windowsV3ObjectFacts,
	expected windowsV3VolumeIdentity,
) error {
	// The retained root already certified the volume. Each ancestry handle must
	// independently prove that same GUID and serial before reusing its capabilities.
	if facts.object.volume != expected {
		return windowsV3Failure("certify external output placement", facts.path, errWindowsV3OutputUnsafe,
			errors.New("external placement crossed the certified NTFS volume boundary"))
	}
	// External components are spelling/placement authorities, not output lookup
	// roots. Per-directory case-sensitive lookup therefore does not affect the
	// handle-bound output namespace beneath them.
	return validateWindowsV3DirectoryShape(
		facts, "certify external output placement", "external output placement",
	)
}

func validateWindowsV3VolumeCertification(facts windowsV3VolumeFacts) error {
	switch {
	case !strings.EqualFold(facts.filesystem, windowsV3OutputFilesystem):
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			fmt.Errorf("filesystem %q is not NTFS", facts.filesystem))
	case strings.HasPrefix(strings.TrimPrefix(facts.path, `\\?\`), `UNC\`):
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("network filesystems are not certified"))
	case facts.driveType != windows.DRIVE_FIXED:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			fmt.Errorf("drive type %d is not a certified fixed disk", facts.driveType))
	case facts.flags&windows.FILE_SUPPORTS_HARD_LINKS == 0:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume does not report regular-file hard-link support"))
	case facts.flags&windows.FILE_PERSISTENT_ACLS == 0:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume does not report persistent ACL support"))
	case facts.flags&windowsV3FileSupportsPOSIXSemantics == 0:
		return windowsV3Failure("certify output filesystem", facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume does not report handle-bound rename/unlink support"))
	default:
		return nil
	}
}

func validateWindowsV3DirectoryShape(
	facts windowsV3ObjectFacts,
	operation string,
	role string,
) error {
	switch {
	case facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return windowsV3Failure(operation, facts.path, errWindowsV3OutputUnsupported,
			errors.New(role+" is not a directory"))
	case facts.attributes&windowsV3CloudAttributeMask != 0:
		return windowsV3Failure(operation, facts.path, errWindowsV3OutputUnsupported,
			errors.New("reparse, offline, and cloud-placeholder directories are not certified"))
	case !facts.object.valid():
		return windowsV3Failure(operation, facts.path, errWindowsV3OutputUnsupported,
			errors.New("volume or File ID identity is unavailable"))
	default:
		return nil
	}
}
