//go:build windows

package output

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

func filetimeTicks(ft windows.Filetime) int64 {
	return int64(uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime))
}

func TestTimeToWindowsFiletimeRangeAndPrecision(t *testing.T) {
	// Within Go's UnixNano window, conversion must match windows.NsecToFiletime.
	inRange := []time.Time{
		time.Unix(-1, 500_000_000).UTC(),
		time.Unix(0, 0).UTC(),
		time.Unix(1_700_000_000, 123_456_700).UTC(),
		time.Date(2262, time.April, 11, 23, 47, 16, 854_775_800, time.UTC),
	}
	for _, ts := range inRange {
		got, err := timeToWindowsFiletime(ts)
		if err != nil {
			t.Fatalf("timeToWindowsFiletime(%v): %v", ts, err)
		}
		want := windows.NsecToFiletime(ts.UnixNano())
		if got != want {
			t.Fatalf("timeToWindowsFiletime(%v) = %+v, want %+v", ts, got, want)
		}
	}

	// Dates outside UnixNano (1678-2262) that fit signed Windows FILETIME must
	// convert accurately without wrapping.
	wideCases := []struct {
		name      string
		t         time.Time
		wantTicks int64
	}{
		{
			name:      "minimum positive FILETIME tick",
			t:         time.Unix(windowsFiletimeEpochUnixSeconds, 100).UTC(),
			wantTicks: 1,
		},
		{
			name:      "pre-1678 date",
			t:         time.Date(1650, time.June, 15, 12, 34, 56, 789_012_300, time.UTC),
			wantTicks: (time.Date(1650, time.June, 15, 12, 34, 56, 0, time.UTC).Unix()-windowsFiletimeEpochUnixSeconds)*windowsTicksPerSecond + 7_890_123,
		},
		{
			name:      "post-2262 date",
			t:         time.Date(2500, time.January, 1, 0, 0, 0, 123_456_700, time.UTC),
			wantTicks: (time.Date(2500, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()-windowsFiletimeEpochUnixSeconds)*windowsTicksPerSecond + 1_234_567,
		},
		{
			name:      "year 9999 sentinel",
			t:         time.Date(9999, time.December, 31, 23, 59, 59, 999_999_900, time.UTC),
			wantTicks: (time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC).Unix()-windowsFiletimeEpochUnixSeconds)*windowsTicksPerSecond + 9_999_999,
		},
		{
			name:      "maximum signed FILETIME",
			t:         time.Unix(windowsFiletimeMaxUnixSeconds, windowsFiletimeMaxSubsecTicks*windowsNanosPerTick).UTC(),
			wantTicks: math.MaxInt64,
		},
		{
			name:      "maximum signed FILETIME truncates sub-100ns remainder",
			t:         time.Unix(windowsFiletimeMaxUnixSeconds, windowsFiletimeMaxSubsecTicks*windowsNanosPerTick+99).UTC(),
			wantTicks: math.MaxInt64,
		},
	}
	for _, tc := range wideCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := timeToWindowsFiletime(tc.t)
			if err != nil {
				t.Fatalf("timeToWindowsFiletime(%v): %v", tc.t, err)
			}
			if ticks := filetimeTicks(got); ticks != tc.wantTicks {
				t.Fatalf("ticks = %d, want %d", ticks, tc.wantTicks)
			}
		})
	}

	outOfRange := []struct {
		name string
		t    time.Time
	}{
		{name: "FILETIME zero no-op sentinel", t: time.Unix(windowsFiletimeEpochUnixSeconds, 0).UTC()},
		{name: "truncates to FILETIME zero", t: time.Unix(windowsFiletimeEpochUnixSeconds, 99).UTC()},
		{name: "before 1601 epoch", t: time.Unix(windowsFiletimeEpochUnixSeconds-1, 999_999_999).UTC()},
		{name: "one tick past MaxInt64", t: time.Unix(windowsFiletimeMaxUnixSeconds, (windowsFiletimeMaxSubsecTicks+1)*windowsNanosPerTick).UTC()},
		{name: "one second past MaxInt64", t: time.Unix(windowsFiletimeMaxUnixSeconds+1, 0).UTC()},
		{name: "catalog max positive seconds", t: time.Unix(catalog.MaxSafeInteger, 0).UTC()},
		{name: "catalog max negative seconds", t: time.Unix(-catalog.MaxSafeInteger, 0).UTC()},
	}
	for _, tc := range outOfRange {
		t.Run(tc.name, func(t *testing.T) {
			if ft, err := timeToWindowsFiletime(tc.t); !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				t.Fatalf("timeToWindowsFiletime(%v) = (%+v, %v), want ERROR_INVALID_PARAMETER", tc.t, ft, err)
			}
		})
	}
}

func TestWindowsSetModifiedTimeHandlesOutsideUnixNanoRange(t *testing.T) {
	base := t.TempDir()
	targetDir := filepath.Join(base, "dir")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, _, err := OpenContainer(targetDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	file, err := openChildFile(dir, "sample.bin", os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	validTimes := []time.Time{
		time.Date(1650, time.June, 15, 12, 34, 56, 789_012_300, time.UTC),
		time.Date(2500, time.January, 1, 0, 0, 0, 123_456_700, time.UTC),
		time.Date(9999, time.December, 31, 23, 59, 59, 999_999_900, time.UTC),
	}
	for _, target := range validTimes {
		want, err := timeToWindowsFiletime(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := setFileModifiedTime(file, target); err != nil {
			t.Fatalf("setFileModifiedTime(%v): %v", target, err)
		}
		var fileInfo windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &fileInfo); err != nil {
			t.Fatal(err)
		}
		if fileInfo.LastWriteTime != want {
			t.Fatalf("file LastWriteTime(%v) = %+v, want %+v", target, fileInfo.LastWriteTime, want)
		}

		if err := setDirModifiedTime(dir, target); err != nil {
			t.Fatalf("setDirModifiedTime(%v): %v", target, err)
		}
		var dirInfo windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(windows.Handle(dir.handle.Fd()), &dirInfo); err != nil {
			t.Fatal(err)
		}
		if dirInfo.LastWriteTime != want {
			t.Fatalf("dir LastWriteTime(%v) = %+v, want %+v", target, dirInfo.LastWriteTime, want)
		}
	}

	var beforeFile, beforeDir windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &beforeFile); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(dir.handle.Fd()), &beforeDir); err != nil {
		t.Fatal(err)
	}

	for _, invalid := range []time.Time{
		time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(windowsFiletimeEpochUnixSeconds, 0).UTC(),
		time.Unix(catalog.MaxSafeInteger, 0).UTC(),
	} {
		if err := setFileModifiedTime(file, invalid); !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			t.Fatalf("setFileModifiedTime(%v) = %v, want ERROR_INVALID_PARAMETER", invalid, err)
		}
		if err := setDirModifiedTime(dir, invalid); !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			t.Fatalf("setDirModifiedTime(%v) = %v, want ERROR_INVALID_PARAMETER", invalid, err)
		}
	}

	var afterFile, afterDir windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &afterFile); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(dir.handle.Fd()), &afterDir); err != nil {
		t.Fatal(err)
	}
	if afterFile.LastWriteTime != beforeFile.LastWriteTime || afterDir.LastWriteTime != beforeDir.LastWriteTime {
		t.Fatal("rejected out-of-range timestamp mutated existing LastWriteTime")
	}
}
