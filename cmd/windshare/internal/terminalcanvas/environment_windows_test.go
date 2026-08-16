//go:build windows

package terminalcanvas

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDetectWindowsCapabilitiesEnablesVirtualTerminalMode(t *testing.T) {
	var setMode uint32
	width := 120
	provider := detectWindowsCapabilities(fakeRawTerminal(42), windowsConsoleAPI{
		isTerminal: func(fd int) bool { return fd == 42 },
		getSize: func(fd int) (int, int, error) {
			return width, 24, nil
		},
		getMode: func(handle windows.Handle, mode *uint32) error {
			*mode = windows.ENABLE_PROCESSED_OUTPUT
			return nil
		},
		setMode: func(_ windows.Handle, mode uint32) error {
			setMode = mode
			return nil
		},
		getOutputCP: func() (uint32, error) { return windowsUTF8CodePage, nil },
		lookupEnv:   func(string) (string, bool) { return "", false },
	})

	got := provider.Snapshot()
	wantMode := uint32(windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	if setMode != wantMode {
		t.Fatalf("SetConsoleMode mode = %#x, want %#x", setMode, wantMode)
	}
	if got != (Capabilities{Interactive: true, ANSI: true, Color: true, Unicode: true, Columns: 120}) {
		t.Fatalf("Snapshot() = %+v", got)
	}

	width = 72
	if got := provider.Snapshot().Columns; got != 72 {
		t.Fatalf("resized Columns = %d, want 72", got)
	}
}

func TestDetectWindowsCapabilitiesKeepsANSIColorAndUnicodeIndependent(t *testing.T) {
	provider := detectWindowsCapabilities(fakeRawTerminal(9), windowsConsoleAPI{
		isTerminal: func(int) bool { return true },
		getSize: func(int) (int, int, error) {
			return 0, 0, errors.New("unavailable")
		},
		getMode:     func(windows.Handle, *uint32) error { return nil },
		setMode:     func(windows.Handle, uint32) error { return errors.New("unsupported") },
		getOutputCP: func() (uint32, error) { return windowsUTF8CodePage, nil },
		lookupEnv:   func(string) (string, bool) { return "", false },
	})

	got := provider.Snapshot()
	if !got.Interactive || got.ANSI || got.Color || !got.Unicode || got.Columns != 0 {
		t.Fatalf("Snapshot() coupled independent facts: %+v", got)
	}
}

func TestDetectWindowsCapabilitiesHonorsExistingModeAndNoColor(t *testing.T) {
	setModeCalled := false
	provider := detectWindowsCapabilities(fakeRawTerminal(7), windowsConsoleAPI{
		isTerminal: func(int) bool { return true },
		getSize:    func(int) (int, int, error) { return 80, 25, nil },
		getMode: func(_ windows.Handle, mode *uint32) error {
			*mode = windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
			return nil
		},
		setMode: func(windows.Handle, uint32) error {
			setModeCalled = true
			return nil
		},
		getOutputCP: func() (uint32, error) { return 437, nil },
		lookupEnv: func(name string) (string, bool) {
			return "", name == "NO_COLOR"
		},
	})

	got := provider.Snapshot()
	if setModeCalled {
		t.Fatal("existing virtual-terminal mode was unnecessarily reset")
	}
	if !got.ANSI || got.Color || got.Unicode {
		t.Fatalf("Snapshot() = %+v", got)
	}
}
