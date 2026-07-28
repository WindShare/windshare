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

func windowsV3OutputLocatorKey(path string) (string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return "", errors.Join(windowsV3Failure("validate output locator", path, errWindowsV3OutputUnsafe,
			errors.New("locator is not a canonical catalog path")), err)
	}
	native, err := windowsV3RelativePath(path, false)
	if err != nil {
		return "", windowsV3Failure("validate output locator", path, errWindowsV3OutputUnsafe, err)
	}
	key, err := windowsV3NTFSCaseKey(native)
	if err != nil {
		return "", windowsV3Failure("canonicalize output locator", path, errWindowsV3OutputUnsupported, err)
	}
	return key, nil
}

func windowsV3NTFSCaseKey(value string) (string, error) {
	if err := windowsV3RtlUpcaseUnicodeChar.Find(); err != nil {
		return "", fmt.Errorf("load Windows ordinal upcase table: %w", err)
	}
	units := utf16.Encode([]rune(value))
	for index, unit := range units {
		upper, _, _ := windowsV3RtlUpcaseUnicodeChar.Call(uintptr(unit))
		units[index] = uint16(upper)
	}
	return string(utf16.Decode(units)), nil
}

func windowsV3VerifyOpenedExactName(handle windows.Handle, expected string) error {
	return windowsV3VerifyOpenedLeafAuthority(handle, expected, true)
}

func windowsV3VerifyOpenedPlacementLeafAuthority(
	handle windows.Handle,
	requested string,
	parentCaseSensitive bool,
) error {
	const operation = "verify opened output placement name"
	native, err := windowsV3RelativePath(requested, true)
	if err != nil {
		return windowsV3Failure(operation, requested, errWindowsV3OutputUnsafe, err)
	}
	normalized, err := windowsV3OpenedLeafNameWithFlags(handle, 0)
	if err != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe, err)
	}
	opened, err := windowsV3OpenedLeafNameWithFlags(handle, windowsV3FinalPathNameOpened)
	if err != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe, err)
	}
	match, compareErr := windowsV3PlacementLeafNamesMatch(
		native, normalized, opened, parentCaseSensitive,
	)
	if compareErr != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsupported, compareErr)
	}
	if !match {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe,
			fmt.Errorf("requested placement resolves to normalized leaf %q through opened leaf %q", normalized, opened))
	}
	return nil
}

func windowsV3PlacementLeafNamesMatch(requested, normalized, opened string, caseSensitive bool) (bool, error) {
	if caseSensitive {
		return requested == normalized || requested == opened, nil
	}
	requestedKey, requestedErr := windowsV3NTFSCaseKey(requested)
	normalizedKey, normalizedErr := windowsV3NTFSCaseKey(normalized)
	openedKey, openedErr := windowsV3NTFSCaseKey(opened)
	if requestedErr != nil || normalizedErr != nil || openedErr != nil {
		return false, errors.Join(requestedErr, normalizedErr, openedErr)
	}
	return requestedKey == normalizedKey || requestedKey == openedKey, nil
}

func windowsV3VerifyOpenedLeafAuthority(handle windows.Handle, expected string, exact bool) error {
	const operation = "verify opened output entry name"
	native, err := windowsV3RelativePath(expected, true)
	if err != nil {
		return windowsV3Failure(operation, expected, errWindowsV3OutputUnsafe, err)
	}
	actual, err := windowsV3OpenedLeafName(handle)
	if err != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe, err)
	}
	if exact && actual != native {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe,
			fmt.Errorf("actual private entry spelling is %q", actual))
	}
	if exact {
		return nil
	}
	// NTFS can resolve a DOS 8.3 alias that is unrelated to the requested
	// catalog component. Comparing the handle-derived long leaf through the
	// volume's ordinal upcase table permits case-only reuse while refusing a
	// different namespace spelling before descent or mutation.
	expectedKey, expectedErr := windowsV3NTFSCaseKey(native)
	actualKey, actualErr := windowsV3NTFSCaseKey(actual)
	if expectedErr != nil || actualErr != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsupported,
			errors.Join(expectedErr, actualErr))
	}
	if actualKey != expectedKey {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe,
			fmt.Errorf("requested entry resolves to different long leaf %q", actual))
	}
	return nil
}

func windowsV3OpenedLeafName(handle windows.Handle) (string, error) {
	return windowsV3OpenedLeafNameWithFlags(handle, 0)
}

func windowsV3OpenedLeafNameWithFlags(handle windows.Handle, flags uint32) (string, error) {
	// Both normalized and opened spellings come from the same handle. Callers can
	// therefore distinguish canonical output lookup from placement-only alias
	// binding without re-resolving an attacker-controlled path.
	normalized, err := windowsV3FinalPath(handle, flags)
	if err != nil {
		return "", fmt.Errorf("query opened file name: %w", err)
	}
	leaf := filepath.Base(normalized)
	if leaf == "." || leaf == string(filepath.Separator) {
		return "", errors.New("opened file name does not contain a leaf component")
	}
	validated, err := windowsV3RelativePath(leaf, true)
	if err != nil {
		return "", fmt.Errorf("validate opened file name: %w", err)
	}
	return validated, nil
}

func windowsV3ValidateModifiedTime(modified catalog.ModifiedTime) error {
	_, _, err := windowsV3ModifiedTimeTicks(modified)
	return err
}

func windowsV3ModifiedTimeTicks(modified catalog.ModifiedTime) (uint64, bool, error) {
	const operation = "validate output modified time"
	if !modified.Present() {
		return 0, false, nil
	}
	if modified.Precision() < catalog.TimePrecisionSeconds || modified.Precision() > catalog.TimePrecisionNanoseconds ||
		modified.Nanoseconds() >= 1_000_000_000 {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("catalog modified time is invalid"))
	}
	if modified.Precision() == catalog.TimePrecisionNanoseconds &&
		modified.Nanoseconds()%windowsV3FiletimeNanosecondsPerTick != 0 {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("nanosecond timestamp is not exactly representable by NTFS FILETIME"))
	}
	shiftedSeconds := modified.Seconds() + windowsV3UnixEpochFiletimeSeconds
	if shiftedSeconds < 0 {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("timestamp predates the NTFS FILETIME epoch"))
	}
	seconds := uint64(shiftedSeconds)
	fraction := uint64(modified.Nanoseconds() / windowsV3FiletimeNanosecondsPerTick)
	if seconds > math.MaxUint64/windowsV3FiletimeTicksPerSecond ||
		seconds*windowsV3FiletimeTicksPerSecond > math.MaxUint64-fraction {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("timestamp exceeds the NTFS FILETIME range"))
	}
	return seconds*windowsV3FiletimeTicksPerSecond + fraction, true, nil
}

func (file *windowsV3File) setModifiedTime(modified catalog.ModifiedTime) error {
	if err := file.verify(false); err != nil {
		return err
	}
	return windowsV3SetHandleModifiedTime(file.handle(), file.path, modified)
}

func (directory *windowsV3Directory) setModifiedTime(modified catalog.ModifiedTime) error {
	if err := directory.verify(false); err != nil {
		return err
	}
	return windowsV3SetHandleModifiedTime(directory.handle(), directory.path, modified)
}

func windowsV3SetHandleModifiedTime(handle windows.Handle, path string, modified catalog.ModifiedTime) error {
	ticks, present, err := windowsV3ModifiedTimeTicks(modified)
	if err != nil || !present {
		return err
	}
	filetime := windows.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(ticks >> 32)}
	if err := windows.SetFileTime(handle, nil, nil, &filetime); err != nil {
		return windowsV3NativeOperationFailure("set output modified time", path, err)
	}
	return nil
}

func (file *windowsV3File) metadataMatches(
	exactSize uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if err := file.verify(false); err != nil {
		return false, err
	}
	metadata, err := windowsV3ReadHandleMetadata(file.handle())
	if err != nil {
		return false, windowsV3Failure("inspect output file metadata", file.path, errWindowsV3OutputUnsafe, err)
	}
	return metadata.size == exactSize && windowsV3ModifiedTimeMatches(metadata.modifiedTicks, modified), nil
}

func windowsV3ReadHandleMetadata(handle windows.Handle) (windowsV3OutputMetadata, error) {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsV3OutputMetadata{}, err
	}
	return windowsV3OutputMetadata{
		size: uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow),
		modifiedTicks: uint64(information.LastWriteTime.HighDateTime)<<32 |
			uint64(information.LastWriteTime.LowDateTime),
	}, nil
}

func windowsV3ModifiedTimeMatches(actual uint64, expected catalog.ModifiedTime) bool {
	ticks, present, err := windowsV3ModifiedTimeTicks(expected)
	if err != nil {
		return false
	}
	if !present {
		return true
	}
	switch expected.Precision() {
	case catalog.TimePrecisionSeconds:
		return actual/windowsV3FiletimeTicksPerSecond == ticks/windowsV3FiletimeTicksPerSecond
	case catalog.TimePrecisionMilliseconds:
		const ticksPerMillisecond = uint64(10_000)
		return actual/ticksPerMillisecond == ticks/ticksPerMillisecond
	case catalog.TimePrecisionNanoseconds:
		return actual == ticks
	default:
		return false
	}
}
