package relaysetup

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumDeviceNameBytes = 128
	fallbackDeviceName     = "WindShare client"
)

// ErrInvalidDeviceName identifies input that cannot be preserved by enrollment
// and checkpoint serialization.
var ErrInvalidDeviceName = fmt.Errorf("device name must be valid UTF-8, 1 to %d bytes, not blank, and free of control characters", maximumDeviceNameBytes)

// ValidateDeviceName shares the enrollment rule with callers that can reject
// input before starting setup or contacting a relay.
func ValidateDeviceName(name string) error {
	// JSON replaces invalid UTF-8, which can expand an accepted name past the
	// byte limit and make the committed checkpoint impossible to read back.
	if len(name) > maximumDeviceNameBytes || !utf8.ValidString(name) ||
		strings.TrimSpace(name) == "" || strings.ContainsFunc(name, unicode.IsControl) {
		return ErrInvalidDeviceName
	}
	return nil
}

// DefaultDeviceName makes an environment-provided display label usable without
// user intervention. Explicit names must be validated without rewriting them.
func DefaultDeviceName(suggestion string) string {
	name := strings.TrimSpace(strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return -1
		}
		return value
	}, strings.ToValidUTF8(suggestion, "")))
	if len(name) > maximumDeviceNameBytes {
		end := maximumDeviceNameBytes
		for !utf8.RuneStart(name[end]) {
			end--
		}
		name = strings.TrimSpace(name[:end])
	}
	if name == "" {
		return fallbackDeviceName
	}
	return name
}
