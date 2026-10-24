//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"unicode/utf16"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

func windowsOutputLocatorKey(path string) (string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return "", errors.Join(windowsFailure("validate output locator", path, errWindowsOutputUnsafe,
			errors.New("locator is not a canonical catalog path")), err)
	}
	native, err := windowsRelativePath(path, false)
	if err != nil {
		return "", windowsFailure("validate output locator", path, errWindowsOutputUnsafe, err)
	}
	key, err := windowsNTFSCaseKey(native)
	if err != nil {
		return "", windowsFailure("canonicalize output locator", path, errWindowsOutputUnsupported, err)
	}
	return key, nil
}

func windowsNTFSCaseKey(value string) (string, error) {
	if err := windowsRtlUpcaseUnicodeChar.Find(); err != nil {
		return "", fmt.Errorf("load Windows ordinal upcase table: %w", err)
	}
	units := utf16.Encode([]rune(value))
	for index, unit := range units {
		upper, _, _ := windowsRtlUpcaseUnicodeChar.Call(uintptr(unit))
		units[index] = uint16(upper)
	}
	return string(utf16.Decode(units)), nil
}

func windowsVerifyOpenedExactName(handle windows.Handle, expected string) error {
	return windowsVerifyOpenedLeafAuthority(handle, expected, true)
}

func windowsVerifyOpenedPlacementLeafAuthority(
	handle windows.Handle,
	requested string,
	parentCaseSensitive bool,
) error {
	const operation = "verify opened output placement name"
	native, err := windowsRelativePath(requested, true)
	if err != nil {
		return windowsFailure(operation, requested, errWindowsOutputUnsafe, err)
	}
	normalized, err := windowsOpenedLeafNameWithFlags(handle, 0)
	if err != nil {
		return windowsFailure(operation, native, errWindowsOutputUnsafe, err)
	}
	opened, err := windowsOpenedLeafNameWithFlags(handle, windowsFinalPathNameOpened)
	if err != nil {
		return windowsFailure(operation, native, errWindowsOutputUnsafe, err)
	}
	match, compareErr := windowsPlacementLeafNamesMatch(
		native, normalized, opened, parentCaseSensitive,
	)
	if compareErr != nil {
		return windowsFailure(operation, native, errWindowsOutputUnsupported, compareErr)
	}
	if !match {
		return windowsFailure(operation, native, errWindowsOutputUnsafe,
			fmt.Errorf("requested placement resolves to normalized leaf %q through opened leaf %q", normalized, opened))
	}
	return nil
}

func windowsPlacementLeafNamesMatch(requested, normalized, opened string, caseSensitive bool) (bool, error) {
	if caseSensitive {
		return requested == normalized || requested == opened, nil
	}
	requestedKey, requestedErr := windowsNTFSCaseKey(requested)
	normalizedKey, normalizedErr := windowsNTFSCaseKey(normalized)
	openedKey, openedErr := windowsNTFSCaseKey(opened)
	if requestedErr != nil || normalizedErr != nil || openedErr != nil {
		return false, errors.Join(requestedErr, normalizedErr, openedErr)
	}
	return requestedKey == normalizedKey || requestedKey == openedKey, nil
}

func windowsVerifyOpenedLeafAuthority(handle windows.Handle, expected string, exact bool) error {
	const operation = "verify opened output entry name"
	native, err := windowsRelativePath(expected, true)
	if err != nil {
		return windowsFailure(operation, expected, errWindowsOutputUnsafe, err)
	}
	actual, err := windowsOpenedLeafName(handle)
	if err != nil {
		return windowsFailure(operation, native, errWindowsOutputUnsafe, err)
	}
	if exact && actual != native {
		return windowsFailure(operation, native, errWindowsOutputUnsafe,
			fmt.Errorf("actual private entry spelling is %q", actual))
	}
	if exact {
		return nil
	}
	// NTFS can resolve a DOS 8.3 alias that is unrelated to the requested
	// catalog component. Comparing the handle-derived long leaf through the
	// volume's ordinal upcase table permits case-only reuse while refusing a
	// different namespace spelling before descent or mutation.
	expectedKey, expectedErr := windowsNTFSCaseKey(native)
	actualKey, actualErr := windowsNTFSCaseKey(actual)
	if expectedErr != nil || actualErr != nil {
		return windowsFailure(operation, native, errWindowsOutputUnsupported,
			errors.Join(expectedErr, actualErr))
	}
	if actualKey != expectedKey {
		return windowsFailure(operation, native, errWindowsOutputUnsafe,
			fmt.Errorf("requested entry resolves to different long leaf %q", actual))
	}
	return nil
}

func windowsOpenedLeafName(handle windows.Handle) (string, error) {
	return windowsOpenedLeafNameWithFlags(handle, 0)
}

func windowsOpenedLeafNameWithFlags(handle windows.Handle, flags uint32) (string, error) {
	// Both normalized and opened spellings come from the same handle. Callers can
	// therefore distinguish canonical output lookup from placement-only alias
	// binding without re-resolving an attacker-controlled path.
	normalized, err := windowsFinalPath(handle, flags)
	if err != nil {
		return "", fmt.Errorf("query opened file name: %w", err)
	}
	leaf := filepath.Base(normalized)
	if leaf == "." || leaf == string(filepath.Separator) {
		return "", errors.New("opened file name does not contain a leaf component")
	}
	validated, err := windowsRelativePath(leaf, true)
	if err != nil {
		return "", fmt.Errorf("validate opened file name: %w", err)
	}
	return validated, nil
}

func windowsValidateModifiedTime(modified catalog.ModifiedTime) error {
	_, _, err := windowsModifiedTimeTicks(modified)
	return err
}

func windowsModifiedTimeTicks(modified catalog.ModifiedTime) (uint64, bool, error) {
	const operation = "validate output modified time"
	if !modified.Present() {
		return 0, false, nil
	}
	if modified.Precision() < catalog.TimePrecisionSeconds || modified.Precision() > catalog.TimePrecisionNanoseconds ||
		modified.Nanoseconds() >= 1_000_000_000 {
		return 0, false, windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New("catalog modified time is invalid"))
	}
	if modified.Precision() == catalog.TimePrecisionNanoseconds &&
		modified.Nanoseconds()%windowsFiletimeNanosecondsPerTick != 0 {
		return 0, false, windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New("nanosecond timestamp is not exactly representable by NTFS FILETIME"))
	}
	shiftedSeconds := modified.Seconds() + windowsUnixEpochFiletimeSeconds
	if shiftedSeconds < 0 {
		return 0, false, windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New("timestamp predates the NTFS FILETIME epoch"))
	}
	seconds := uint64(shiftedSeconds)
	fraction := uint64(modified.Nanoseconds() / windowsFiletimeNanosecondsPerTick)
	if seconds > math.MaxUint64/windowsFiletimeTicksPerSecond ||
		seconds*windowsFiletimeTicksPerSecond > math.MaxUint64-fraction {
		return 0, false, windowsFailure(operation, "", errWindowsOutputUnsupported,
			errors.New("timestamp exceeds the NTFS FILETIME range"))
	}
	return seconds*windowsFiletimeTicksPerSecond + fraction, true, nil
}

func (file *windowsFile) setModifiedTime(modified catalog.ModifiedTime) error {
	if err := file.verify(false); err != nil {
		return err
	}
	return windowsSetHandleModifiedTime(file.handle(), file.path, modified)
}

func (directory *windowsDirectory) setModifiedTime(modified catalog.ModifiedTime) error {
	if err := directory.verify(false); err != nil {
		return err
	}
	if directory.private {
		return windowsSetHandleModifiedTime(directory.handle(), directory.path, modified)
	}
	if directory.metadataHandleOpen {
		return windowsSetHandleModifiedTime(directory.metadataHandle, directory.path, modified)
	}
	if directory.metadataOpenErr != nil {
		return directory.metadataOpenErr
	}
	return windowsNativeOperationFailure(
		"set output modified time", directory.path, windows.ERROR_ACCESS_DENIED,
	)
}

func windowsSetHandleModifiedTime(handle windows.Handle, path string, modified catalog.ModifiedTime) error {
	ticks, present, err := windowsModifiedTimeTicks(modified)
	if err != nil || !present {
		return err
	}
	filetime := windows.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(ticks >> 32)}
	if err := windows.SetFileTime(handle, nil, nil, &filetime); err != nil {
		return windowsNativeOperationFailure("set output modified time", path, err)
	}
	return nil
}

func (file *windowsFile) metadataMatches(
	exactSize uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if err := file.verify(false); err != nil {
		return false, err
	}
	metadata, err := windowsReadHandleMetadata(file.handle())
	if err != nil {
		return false, windowsFailure("inspect output file metadata", file.path, errWindowsOutputUnsafe, err)
	}
	return metadata.size == exactSize && windowsModifiedTimeMatches(metadata.modifiedTicks, modified), nil
}

func windowsReadHandleMetadata(handle windows.Handle) (windowsOutputMetadata, error) {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsOutputMetadata{}, err
	}
	return windowsOutputMetadata{
		size: uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow),
		modifiedTicks: uint64(information.LastWriteTime.HighDateTime)<<32 |
			uint64(information.LastWriteTime.LowDateTime),
	}, nil
}

func windowsModifiedTimeMatches(actual uint64, expected catalog.ModifiedTime) bool {
	ticks, present, err := windowsModifiedTimeTicks(expected)
	if err != nil {
		return false
	}
	if !present {
		return true
	}
	switch expected.Precision() {
	case catalog.TimePrecisionSeconds:
		return actual/windowsFiletimeTicksPerSecond == ticks/windowsFiletimeTicksPerSecond
	case catalog.TimePrecisionMilliseconds:
		const ticksPerMillisecond = uint64(10_000)
		return actual/ticksPerMillisecond == ticks/ticksPerMillisecond
	case catalog.TimePrecisionNanoseconds:
		return actual == ticks
	default:
		return false
	}
}
