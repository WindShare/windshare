package testicetopology

import (
	"strings"
	"testing"
)

func TestCanonicalJSONParserAcceptsExactIntegerJSONLanguage(t *testing.T) {
	t.Parallel()
	encoded := []byte(" \t\r\n{\"emptyObject\":{},\"emptyArray\":[],\"values\":[true,false,null,0,-1,9007199254740991,-9007199254740991,\"\\\"\\\\\\/\\b\\f\\n\\r\\t\\u0061\\ud83d\\ude00\"]} \n")
	if err := validateCanonicalJSON(encoded, "valid fixture"); err != nil {
		t.Fatalf("validateCanonicalJSON: %v", err)
	}
	var target struct {
		EmptyObject map[string]any `json:"emptyObject"`
		EmptyArray  []any          `json:"emptyArray"`
		Values      []any          `json:"values"`
	}
	if err := decodeCanonicalJSON(encoded, "valid fixture", &target); err != nil {
		t.Fatalf("decodeCanonicalJSON: %v", err)
	}
}

func TestCanonicalJSONParserRejectsNonCanonicalInputs(t *testing.T) {
	t.Parallel()
	invalidUTF8 := []byte{'"', 0xff, '"'}
	tests := map[string][]byte{
		"empty":                     {},
		"incomplete array":          []byte(`[`),
		"incomplete object":         []byte(`{`),
		"unquoted member":           []byte(`{a:1}`),
		"missing colon":             []byte(`{"a" 1}`),
		"missing object comma":      []byte(`{"a":1 "b":2}`),
		"trailing object comma":     []byte(`{"a":1,}`),
		"missing array comma":       []byte(`[1 2]`),
		"trailing array comma":      []byte(`[1,]`),
		"duplicate decoded member":  []byte(`{"a":1,"\u0061":2}`),
		"raw control in string":     []byte("\"line\nbreak\""),
		"unterminated string":       []byte(`"unterminated`),
		"unterminated escape":       []byte(`"unterminated\`),
		"invalid escape":            []byte(`"\x"`),
		"incomplete Unicode escape": []byte(`"\u123"`),
		"invalid Unicode escape":    []byte(`"\u12xz"`),
		"unpaired high surrogate":   []byte(`"\ud800"`),
		"wrong low surrogate":       []byte(`"\ud800\u0041"`),
		"unpaired low surrogate":    []byte(`"\udfff"`),
		"raw replacement character": []byte("\"\ufffd\""),
		"escaped replacement":       []byte(`"\ufffd"`),
		"negative zero":             []byte(`-0`),
		"bare minus":                []byte(`-`),
		"leading plus":              []byte(`+1`),
		"leading zero":              []byte(`01`),
		"decimal":                   []byte(`1.0`),
		"exponent":                  []byte(`1e0`),
		"unsafe positive integer":   []byte(`9007199254740992`),
		"unsafe negative integer":   []byte(`-9007199254740992`),
		"overflowing integer":       []byte(`999999999999999999999999999999`),
		"invalid literal":           []byte(`truth`),
		"incomplete literal":        []byte(`tru`),
		"non-JSON whitespace":       []byte("\vnull"),
		"trailing data":             []byte(`null null`),
		"invalid UTF-8":             invalidUTF8,
	}
	for name, encoded := range tests {
		name, encoded := name, encoded
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateCanonicalJSON(encoded, "invalid fixture"); err == nil {
				t.Fatalf("validateCanonicalJSON(%q) succeeded", encoded)
			}
		})
	}
}

func TestCanonicalJSONWriterMatchesJSONStringifyEscapes(t *testing.T) {
	t.Parallel()
	value := "\"\\\b\f\n\r\t\x00\x01\x1f/<>&\u2028\u2029😀"
	var writer canonicalJSONWriter
	writer.string(value)
	want := `"\"\\\b\f\n\r\t\u0000\u0001\u001f/<>&` + "\u2028\u2029😀" + `"`
	if writer.String() != want {
		t.Fatalf("canonical string = %q, want %q", writer.String(), want)
	}
	if err := validateCanonicalJSON(writer.Bytes(), "canonical string"); err != nil {
		t.Fatalf("validate canonical string: %v", err)
	}
}

func TestDecodeCanonicalJSONRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var target struct {
		Known int `json:"known"`
	}
	if err := decodeCanonicalJSON([]byte(`{"known":1,"unknown":2}`), "target", &target); err == nil {
		t.Fatal("decodeCanonicalJSON accepted an unknown field")
	}
	if err := decodeCanonicalJSON([]byte(`{"known":1}`), "target", &target); err != nil {
		t.Fatalf("decodeCanonicalJSON valid target: %v", err)
	}
	if target.Known != 1 {
		t.Fatalf("target = %+v", target)
	}
}

func TestValidNFCTextRejectsReplacementCharacter(t *testing.T) {
	t.Parallel()
	if validNFCText("route\ufffdname", 255) {
		t.Fatal("validNFCText accepted the Unicode replacement character")
	}
	if !validNFCText(strings.Repeat("a", 255), 255) {
		t.Fatal("validNFCText rejected a value at the byte limit")
	}
}
