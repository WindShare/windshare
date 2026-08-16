package terminalcanvas

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// EscapeText makes arbitrary display text inert before styling and measurement.
// Visible ASCII escapes preserve diagnostic meaning without letting user values
// move the cursor, split records, or change bidi ordering.
func EscapeText(text string) string {
	var escaped strings.Builder
	escaped.Grow(len(text))

	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r == utf8.RuneError && size == 1 {
			appendByteEscape(&escaped, text[0])
			text = text[1:]
			continue
		}

		switch r {
		case '\n':
			escaped.WriteString(`\n`)
		case '\r':
			escaped.WriteString(`\r`)
		case '\t':
			escaped.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				appendByteEscape(&escaped, byte(r))
			case r >= 0x80 && r <= 0x9f:
				appendUnicodeEscape(&escaped, r)
			case isBidiControl(r) || r == '\u2028' || r == '\u2029':
				appendUnicodeEscape(&escaped, r)
			default:
				escaped.WriteRune(r)
			}
		}
		text = text[size:]
	}

	return escaped.String()
}

func appendByteEscape(dst *strings.Builder, value byte) {
	const hexadecimal = "0123456789abcdef"
	dst.WriteString(`\x`)
	dst.WriteByte(hexadecimal[value>>4])
	dst.WriteByte(hexadecimal[value&0x0f])
}

func appendUnicodeEscape(dst *strings.Builder, value rune) {
	dst.WriteString(`\u`)
	digits := strconv.FormatInt(int64(value), 16)
	for padding := 4 - len(digits); padding > 0; padding-- {
		dst.WriteByte('0')
	}
	dst.WriteString(digits)
}

func isBidiControl(r rune) bool {
	switch r {
	case '\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069':
		return true
	default:
		return false
	}
}
