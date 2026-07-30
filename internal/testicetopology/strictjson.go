package testicetopology

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maximumSafeJSONInteger = int64(1<<53 - 1)

// validateCanonicalJSON keeps the Go and browser parsers on the same input
// language. encoding/json alone loses duplicate names and accepts number forms
// that JavaScript cannot preserve as exact integer evidence.
func validateCanonicalJSON(encoded []byte, label string) error {
	if !utf8.Valid(encoded) {
		return fmt.Errorf("%s contains invalid UTF-8", label)
	}
	parser := canonicalJSONParser{encoded: encoded, label: label}
	parser.skipWhitespace()
	if err := parser.value(); err != nil {
		return err
	}
	parser.skipWhitespace()
	if parser.offset != len(encoded) {
		return parser.fail("contains trailing data")
	}
	return nil
}

type canonicalJSONParser struct {
	encoded []byte
	label   string
	offset  int
}

func (parser *canonicalJSONParser) value() error {
	if parser.offset >= len(parser.encoded) {
		return parser.fail("contains an incomplete value")
	}
	switch parser.encoded[parser.offset] {
	case '{':
		return parser.object()
	case '[':
		return parser.array()
	case '"':
		_, err := parser.string()
		return err
	case 't':
		return parser.literal("true")
	case 'f':
		return parser.literal("false")
	case 'n':
		return parser.literal("null")
	default:
		return parser.integer()
	}
}

func (parser *canonicalJSONParser) object() error {
	parser.offset++
	parser.skipWhitespace()
	if parser.consume('}') {
		return nil
	}
	names := make(map[string]struct{})
	for {
		if parser.offset >= len(parser.encoded) || parser.encoded[parser.offset] != '"' {
			return parser.fail("object member name must be a string")
		}
		name, err := parser.string()
		if err != nil {
			return err
		}
		if _, duplicate := names[name]; duplicate {
			return parser.fail("contains duplicate object member " + strconv.Quote(name))
		}
		names[name] = struct{}{}
		parser.skipWhitespace()
		if !parser.consume(':') {
			return parser.fail("object member lacks a colon")
		}
		parser.skipWhitespace()
		if err := parser.value(); err != nil {
			return err
		}
		parser.skipWhitespace()
		if parser.consume('}') {
			return nil
		}
		if !parser.consume(',') {
			return parser.fail("object members must be comma-separated")
		}
		parser.skipWhitespace()
	}
}

func (parser *canonicalJSONParser) array() error {
	parser.offset++
	parser.skipWhitespace()
	if parser.consume(']') {
		return nil
	}
	for {
		if err := parser.value(); err != nil {
			return err
		}
		parser.skipWhitespace()
		if parser.consume(']') {
			return nil
		}
		if !parser.consume(',') {
			return parser.fail("array entries must be comma-separated")
		}
		parser.skipWhitespace()
	}
}

func (parser *canonicalJSONParser) string() (string, error) {
	parser.offset++
	var decoded strings.Builder
	for parser.offset < len(parser.encoded) {
		character := parser.encoded[parser.offset]
		if character == '"' {
			parser.offset++
			return decoded.String(), nil
		}
		if character < 0x20 {
			return "", parser.fail("contains an unescaped string control character")
		}
		if character != '\\' {
			r, size := utf8.DecodeRune(parser.encoded[parser.offset:])
			if r == utf8.RuneError {
				return "", parser.fail("contains the Unicode replacement character")
			}
			decoded.WriteRune(r)
			parser.offset += size
			continue
		}

		parser.offset++
		if parser.offset >= len(parser.encoded) {
			return "", parser.fail("contains an unterminated string escape")
		}
		escape := parser.encoded[parser.offset]
		parser.offset++
		switch escape {
		case '"', '\\', '/':
			decoded.WriteByte(escape)
		case 'b':
			decoded.WriteByte('\b')
		case 'f':
			decoded.WriteByte('\f')
		case 'n':
			decoded.WriteByte('\n')
		case 'r':
			decoded.WriteByte('\r')
		case 't':
			decoded.WriteByte('\t')
		case 'u':
			unit, err := parser.unicodeUnit()
			if err != nil {
				return "", err
			}
			if unit >= 0xd800 && unit <= 0xdbff {
				if parser.offset+2 > len(parser.encoded) ||
					parser.encoded[parser.offset] != '\\' || parser.encoded[parser.offset+1] != 'u' {
					return "", parser.fail("contains an unpaired Unicode surrogate")
				}
				parser.offset += 2
				low, lowErr := parser.unicodeUnit()
				if lowErr != nil {
					return "", lowErr
				}
				if low < 0xdc00 || low > 0xdfff {
					return "", parser.fail("contains an unpaired Unicode surrogate")
				}
				decoded.WriteRune(utf16.DecodeRune(rune(unit), rune(low)))
			} else if unit >= 0xdc00 && unit <= 0xdfff {
				return "", parser.fail("contains an unpaired Unicode surrogate")
			} else if unit == 0xfffd {
				return "", parser.fail("contains the Unicode replacement character")
			} else {
				decoded.WriteRune(rune(unit))
			}
		default:
			return "", parser.fail("contains an invalid string escape")
		}
	}
	return "", parser.fail("contains an unterminated string")
}

func (parser *canonicalJSONParser) unicodeUnit() (uint16, error) {
	if parser.offset+4 > len(parser.encoded) {
		return 0, parser.fail("contains an incomplete Unicode escape")
	}
	var value uint16
	for range 4 {
		digit := hexValue(parser.encoded[parser.offset])
		if digit < 0 {
			return 0, parser.fail("contains an invalid Unicode escape")
		}
		value = value<<4 | uint16(digit)
		parser.offset++
	}
	return value, nil
}

func hexValue(character byte) int {
	switch {
	case character >= '0' && character <= '9':
		return int(character - '0')
	case character >= 'a' && character <= 'f':
		return int(character-'a') + 10
	case character >= 'A' && character <= 'F':
		return int(character-'A') + 10
	default:
		return -1
	}
}

func (parser *canonicalJSONParser) integer() error {
	start := parser.offset
	negative := parser.consume('-')
	if negative && parser.offset >= len(parser.encoded) {
		return parser.fail("contains a non-canonical integer token")
	}
	if parser.offset >= len(parser.encoded) || parser.encoded[parser.offset] < '0' || parser.encoded[parser.offset] > '9' {
		return parser.fail("contains a non-canonical integer token")
	}
	if parser.encoded[parser.offset] == '0' {
		parser.offset++
		if negative {
			return parser.fail("contains negative zero")
		}
	} else {
		for parser.offset < len(parser.encoded) &&
			parser.encoded[parser.offset] >= '0' && parser.encoded[parser.offset] <= '9' {
			parser.offset++
		}
	}
	if parser.offset < len(parser.encoded) {
		next := parser.encoded[parser.offset]
		if next == '.' || next == 'e' || next == 'E' || next == '+' ||
			(next >= '0' && next <= '9') {
			return parser.fail("contains a non-canonical integer token")
		}
	}
	value, err := strconv.ParseInt(string(parser.encoded[start:parser.offset]), 10, 64)
	if err != nil || value < -maximumSafeJSONInteger || value > maximumSafeJSONInteger {
		return parser.fail("contains an unsafe integer")
	}
	return nil
}

func (parser *canonicalJSONParser) literal(literal string) error {
	if !bytes.HasPrefix(parser.encoded[parser.offset:], []byte(literal)) {
		return parser.fail("contains an invalid literal")
	}
	parser.offset += len(literal)
	return nil
}

func (parser *canonicalJSONParser) skipWhitespace() {
	for parser.offset < len(parser.encoded) {
		switch parser.encoded[parser.offset] {
		case '\t', '\n', '\r', ' ':
			parser.offset++
		default:
			return
		}
	}
}

func (parser *canonicalJSONParser) consume(expected byte) bool {
	if parser.offset >= len(parser.encoded) || parser.encoded[parser.offset] != expected {
		return false
	}
	parser.offset++
	return true
}

func (parser *canonicalJSONParser) fail(reason string) error {
	return fmt.Errorf("%s %s at UTF-8 offset %d", parser.label, reason, parser.offset)
}

func decodeCanonicalJSON(encoded []byte, label string, target any) error {
	if err := validateCanonicalJSON(encoded, label); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode %s trailing data: %w", label, err)
	}
	return nil
}

type canonicalJSONWriter struct {
	bytes.Buffer
}

func (writer *canonicalJSONWriter) string(value string) {
	writer.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			writer.WriteString(`\"`)
		case '\\':
			writer.WriteString(`\\`)
		case '\b':
			writer.WriteString(`\b`)
		case '\f':
			writer.WriteString(`\f`)
		case '\n':
			writer.WriteString(`\n`)
		case '\r':
			writer.WriteString(`\r`)
		case '\t':
			writer.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(writer, `\u%04x`, r)
			} else {
				writer.WriteRune(r)
			}
		}
	}
	writer.WriteByte('"')
}
