//go:build windows

package windowsbroker

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWindowsEnvironmentBlockIsCanonical(t *testing.T) {
	block, err := windowsEnvironmentBlock([]string{"b=2", "A=1"})
	if err != nil {
		t.Fatalf("windowsEnvironmentBlock() error = %v", err)
	}
	if got, want := string(utf16.Decode(block)), "A=1\x00b=2\x00\x00"; got != want {
		t.Fatalf("windowsEnvironmentBlock() = %q, want %q", got, want)
	}
	for _, invalid := range []string{"missing-separator", "bad=entry\x00suffix"} {
		if _, err := windowsEnvironmentBlock([]string{invalid}); err == nil {
			t.Fatalf("windowsEnvironmentBlock(%q) error = nil", invalid)
		}
	}
}

func TestProfileNameValidationKeepsReservedNamespace(t *testing.T) {
	valid := ProfileNamePrefix + strings.Repeat("a", ProfileEntropyHexBytes)
	if !ValidProfileName(valid) {
		t.Fatalf("ValidProfileName(%q) = false", valid)
	}
	for _, invalid := range []string{
		ProfileNamePrefix + strings.Repeat("A", ProfileEntropyHexBytes),
		"Other.Performance." + strings.Repeat("a", ProfileEntropyHexBytes),
	} {
		if ValidProfileName(invalid) {
			t.Fatalf("ValidProfileName(%q) = true", invalid)
		}
	}
}

func TestPathNormalizationRemovesExtendedPrefix(t *testing.T) {
	if got, want := NormalizePath(`\\?\C:\work\file.exe`), `C:\work\file.exe`; got != want {
		t.Fatalf("NormalizePath() = %q, want %q", got, want)
	}
	if got, want := NTPath(`C:\work\file.exe`), `\??\C:\work\file.exe`; got != want {
		t.Fatalf("NTPath() = %q, want %q", got, want)
	}
}
