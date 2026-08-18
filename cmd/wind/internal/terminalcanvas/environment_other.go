//go:build !windows

package terminalcanvas

import (
	"os"
	"strings"

	"golang.org/x/term"
)

type nativeCapabilities struct {
	raw         RawTerminal
	interactive bool
	ansi        bool
	color       bool
	unicode     bool
}

func newNativeCapabilities(raw RawTerminal) CapabilityProvider {
	provider := &nativeCapabilities{raw: raw}
	if raw == nil || !term.IsTerminal(int(raw.Fd())) {
		return provider
	}

	provider.interactive = true
	terminalName, _ := os.LookupEnv("TERM")
	provider.ansi = !strings.EqualFold(terminalName, "dumb")
	provider.color = nativeColorEnabled(provider.ansi, os.LookupEnv)
	provider.unicode = localeSupportsUnicode(os.LookupEnv)
	return provider
}

func (provider *nativeCapabilities) Snapshot() Capabilities {
	columns := 0
	if provider.interactive {
		if width, _, err := term.GetSize(int(provider.raw.Fd())); err == nil && width > 0 {
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
