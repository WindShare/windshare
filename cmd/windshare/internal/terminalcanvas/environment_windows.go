//go:build windows

package terminalcanvas

import (
	"os"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

const windowsUTF8CodePage = 65001

type nativeCapabilities struct {
	raw         RawTerminal
	getSize     func(int) (int, int, error)
	interactive bool
	ansi        bool
	color       bool
	unicode     bool
}

func newNativeCapabilities(raw RawTerminal) CapabilityProvider {
	return detectWindowsCapabilities(raw, windowsConsoleAPI{
		isTerminal:  term.IsTerminal,
		getSize:     term.GetSize,
		getMode:     windows.GetConsoleMode,
		setMode:     windows.SetConsoleMode,
		getOutputCP: windows.GetConsoleOutputCP,
		lookupEnv:   os.LookupEnv,
	})
}

type windowsConsoleAPI struct {
	isTerminal  func(int) bool
	getSize     func(int) (int, int, error)
	getMode     func(windows.Handle, *uint32) error
	setMode     func(windows.Handle, uint32) error
	getOutputCP func() (uint32, error)
	lookupEnv   environmentLookup
}

func detectWindowsCapabilities(raw RawTerminal, console windowsConsoleAPI) CapabilityProvider {
	provider := &nativeCapabilities{raw: raw, getSize: console.getSize}
	if raw == nil || !console.isTerminal(int(raw.Fd())) {
		return provider
	}

	provider.interactive = true
	handle := windows.Handle(raw.Fd())
	var mode uint32
	if console.getMode(handle, &mode) == nil {
		if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 ||
			console.setMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil {
			provider.ansi = true
		}
	}
	provider.color = nativeColorEnabled(provider.ansi, console.lookupEnv)
	codePage, err := console.getOutputCP()
	provider.unicode = err == nil && codePage == windowsUTF8CodePage
	return provider
}

func (provider *nativeCapabilities) Snapshot() Capabilities {
	columns := 0
	if provider.interactive {
		if width, _, err := provider.getSize(int(provider.raw.Fd())); err == nil && width > 0 {
			columns = width
		}
	}
	return Capabilities{
		Interactive: provider.interactive,
		ANSI:        provider.ansi,
		Color:       provider.color,
		Unicode:     provider.unicode,
		Columns:     columns,
	}
}
