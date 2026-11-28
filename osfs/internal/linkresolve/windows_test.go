//go:build windows

package linkresolve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNormalizeWindowsFinalPath(t *testing.T) {
	longSeg := strings.Repeat("a", 70)
	longDrive := filepath.Join(`C:\base`, longSeg, longSeg, longSeg, longSeg)
	longUNC := `\\server\share\` + strings.Join([]string{longSeg, longSeg, longSeg, longSeg}, `\`)
	volumeGUID := `\\?\Volume{11111111-1111-1111-1111-111111111111}\dir`

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "short drive", in: `\\?\C:\dir`, want: `C:\dir`},
		{name: "long drive", in: WindowsExtendedPrefix + longDrive, want: longDrive},
		{name: "short UNC", in: `\\?\UNC\server\share\dir`, want: `\\server\share\dir`},
		{name: "long UNC", in: WindowsExtendedUNCPrefix + strings.TrimPrefix(longUNC, `\\`), want: longUNC},
		{name: "volume GUID preserved", in: volumeGUID, want: volumeGUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeWindowsFinalPath(tc.in); got != tc.want {
				t.Fatalf("NormalizeWindowsFinalPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWindowsFinalPathByHandleWithFallbackAndErrors(t *testing.T) {
	const volumeGUID = `\\?\Volume{11111111-1111-1111-1111-111111111111}\data`

	// 1. Standard DOS path succeeds on the first query and strips \\?\.
	var seenFlags []uint32
	got, err := WindowsFinalPathByHandleWith(func(flags uint32) (string, error) {
		seenFlags = append(seenFlags, flags)
		return `\\?\D:\shared\dir`, nil
	})
	if err != nil || got != `D:\shared\dir` || len(seenFlags) != 1 || seenFlags[0] != WindowsVolumeNameDOS {
		t.Fatalf("DOS path = (%q, %v, flags=%v)", got, err, seenFlags)
	}

	// 2. ERROR_PATH_NOT_FOUND (unlettered mounted volume) falls back to VOLUME_NAME_GUID.
	seenFlags = nil
	got, err = WindowsFinalPathByHandleWith(func(flags uint32) (string, error) {
		seenFlags = append(seenFlags, flags)
		if flags == WindowsVolumeNameDOS {
			return "", windows.ERROR_PATH_NOT_FOUND
		}
		return volumeGUID, nil
	})
	if err != nil || got != volumeGUID || len(seenFlags) != 2 ||
		seenFlags[0] != WindowsVolumeNameDOS || seenFlags[1] != WindowsVolumeNameGUID {
		t.Fatalf("GUID fallback = (%q, %v, flags=%v)", got, err, seenFlags)
	}

	// 3. Non-ERROR_PATH_NOT_FOUND failure propagates without falling back.
	seenFlags = nil
	if _, err := WindowsFinalPathByHandleWith(func(flags uint32) (string, error) {
		seenFlags = append(seenFlags, flags)
		return "", windows.ERROR_ACCESS_DENIED
	}); !errors.Is(err, windows.ERROR_ACCESS_DENIED) || len(seenFlags) != 1 {
		t.Fatalf("access denied = (%v, flags=%v)", err, seenFlags)
	}

	// 4. GUID query failure propagates cleanly.
	if _, err := WindowsFinalPathByHandleWith(func(flags uint32) (string, error) {
		if flags == WindowsVolumeNameDOS {
			return "", windows.ERROR_PATH_NOT_FOUND
		}
		return "", windows.ERROR_INVALID_FUNCTION
	}); !errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		t.Fatalf("GUID failure = %v, want ERROR_INVALID_FUNCTION", err)
	}
}

func TestHasWindowsExtendedOrDevicePrefixAndUnresolvableErrors(t *testing.T) {
	for _, path := range []string{`\\?\C:\dir`, `//?/C:/dir`, `\??\C:\dir`, `\\.\NUL`, `\\?\Volume{1}\`} {
		if !HasWindowsExtendedOrDevicePrefix(path) {
			t.Fatalf("HasWindowsExtendedOrDevicePrefix(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"", `C:\dir`, `\\server\share`, `\\?`} {
		if HasWindowsExtendedOrDevicePrefix(path) {
			t.Fatalf("HasWindowsExtendedOrDevicePrefix(%q) = true, want false", path)
		}
	}

	for _, err := range []error{
		windows.ERROR_CANT_RESOLVE_FILENAME,
		windows.ERROR_INVALID_REPARSE_DATA,
		windows.ERROR_REPARSE_TAG_INVALID,
		windows.ERROR_REPARSE_TAG_MISMATCH,
		windows.ERROR_CANT_ACCESS_FILE,
	} {
		if !IsWindowsUnresolvableLinkError(err) {
			t.Fatalf("IsWindowsUnresolvableLinkError(%v) = false, want true", err)
		}
	}
	for _, err := range []error{nil, windows.ERROR_FILE_NOT_FOUND, windows.ERROR_ACCESS_DENIED} {
		if IsWindowsUnresolvableLinkError(err) {
			t.Fatalf("IsWindowsUnresolvableLinkError(%v) = true, want false", err)
		}
	}
}

func TestWindowsFinalPathByHandleReadsRealDirectoryIncludingLongPath(t *testing.T) {
	base := t.TempDir()
	seg := strings.Repeat("p", 80)
	longDir := filepath.Join(base, seg, seg, seg, seg, seg, seg, seg)
	if len(longDir) <= initialFinalPathUnits {
		t.Fatalf("expected longDir > %d chars to exercise buffer growth, got %d", initialFinalPathUnits, len(longDir))
	}
	if err := os.MkdirAll(longDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path16, err := windows.UTF16PtrFromString(WindowsExtendedPrefix + longDir)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)

	got, err := WindowsFinalPathByHandle(handle)
	if err != nil {
		t.Fatalf("WindowsFinalPathByHandle failed: %v", err)
	}
	if strings.HasPrefix(got, WindowsExtendedPrefix) {
		t.Fatalf("WindowsFinalPathByHandle leaked extended prefix: %q", got)
	}
	wantInfo, err := os.Stat(longDir)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil || !os.SameFile(wantInfo, gotInfo) {
		t.Fatalf("WindowsFinalPathByHandle = %q (err=%v), want same directory as %q", got, err, longDir)
	}
}
