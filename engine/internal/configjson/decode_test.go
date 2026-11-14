package configjson

import (
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRepeatedMembersAcrossEveryObject(t *testing.T) {
	for _, test := range []struct{ input, member, path string }{
		{`{"url":1,"url":2}`, `"url"`, "the top-level object"},
		{`{"profiles":[{"url":1,"URL":2}]}`, `"URL"`, "profiles[0]"},
		{`{"turn":{"accounts":[{"cr\u0065dentials":{"password":"a","PASSWORD":"b"}}]}}`, `"PASSWORD"`, "turn.accounts[0].credentials"},
		{`{"a":[[{"access_key":1,"access_\u212aey":2}]]}`, `"access_\u212aey"`, "a[0][0]"},
	} {
		var target json.RawMessage
		err := Decode([]byte(test.input), &target)
		if !errors.Is(err, ErrRepeatedMember) || !strings.Contains(err.Error(), "member "+test.member+" appears more than once in "+test.path) {
			t.Errorf("unexpected duplicate diagnostic: %v", err)
		}
	}
}

func TestDistinctMembersAndScalars(t *testing.T) {
	for _, input := range []string{
		`{"profiles":[{"url":"a"},{"URL":"b"}]}`, `{"I":1,"ı":2,"İ":3}`,
		`{"number":1e1000,"null":null,"bool":true,"string":"x","array":[],"object":{}}`,
	} {
		var target json.RawMessage
		if err := Decode([]byte(input), &target); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemberFoldingMatchesSimpleCaseEquivalence(t *testing.T) {
	for _, pair := range [][2]string{
		{"url", "URL"}, {"s", "ſ"}, {"k", "K"}, {"i", "İ"},
		{"i", "ı"}, {"Σ", "ς"}, {"σ", "Σ"}, {"ß", "SS"},
	} {
		if (foldMemberName(pair[0]) == foldMemberName(pair[1])) != strings.EqualFold(pair[0], pair[1]) {
			t.Fatalf("folding disagrees with JSON case matching for %q", pair)
		}
	}
}

func TestDecodeRetainsStandardFieldSelectionAndValidation(t *testing.T) {
	type Embedded struct {
		Value string `json:"values"`
	}
	var target struct {
		Values []string `json:"values"`
		Embedded
	}
	if err := Decode([]byte(`{"values":["one"]}`), &target); err != nil || len(target.Values) != 1 {
		t.Fatalf("direct field did not shadow the embedded field: %v", err)
	}
	for _, input := range []string{`{"unknown":1}`, `{"values":{}}`, `{"values":[}`, `{}{}`, `{} garbage`, ""} {
		if err := Decode([]byte(input), &target); err == nil || errors.Is(err, ErrRepeatedMember) {
			t.Fatalf("invalid document accepted or misclassified: %q: %v", input, err)
		}
	}
	var raw json.RawMessage
	deep := strings.Repeat("[", maximumDepth+2) + strings.Repeat("]", maximumDepth+2)
	if err := Decode([]byte(deep), &raw); !errors.Is(err, errDocument) {
		t.Fatal("unbounded nesting accepted:", err)
	}
}

func TestDiagnosticNamesAreBoundedAndEscaped(t *testing.T) {
	for _, name := range []string{"", "x\r\nforged", "a.b", strings.Repeat("n", 4096), strings.Repeat("界", 4096)} {
		input := "{" + strconv.Quote(name) + ":{" + strconv.Quote(name) + ":1," + strconv.Quote(name) + ":2}}"
		var target json.RawMessage
		err := Decode([]byte(input), &target)
		if !errors.Is(err, ErrRepeatedMember) {
			t.Fatal(err)
		}
		if message := err.Error(); strings.ContainsAny(message, "\r\n界") || len(message) > 1024 {
			t.Fatalf("unbounded or multiline diagnostic: %.200q", message)
		}
	}
	// A deeply nested error must not copy or print the entire ancestor chain.
	key := strconv.Quote(strings.Repeat("n", maximumNameRunes))
	input := strings.Repeat("{"+key+":", maximumDepth-1) + `{"x":1,"x":2}` + strings.Repeat("}", maximumDepth-1)
	var target json.RawMessage
	if err := Decode([]byte(input), &target); !errors.Is(err, ErrRepeatedMember) || !strings.Contains(err.Error(), "...") {
		t.Fatal("deep diagnostic was not bounded:", err)
	}
}

func TestScanAllocationDoesNotMultiplyAncestorPaths(t *testing.T) {
	const inputBytes = 64 * 1024
	key := strconv.Quote(strings.Repeat("n", 480))
	prefix := strings.Repeat("{"+key+":", maximumDepth-2) + "[0"
	suffix := "]" + strings.Repeat("}", maximumDepth-2)
	input := prefix + strings.Repeat(",0", (inputBytes-len(prefix)-len(suffix))/2) + suffix
	var target json.RawMessage
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := Decode([]byte(input), &target)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	// The previous path-building implementation allocated hundreds of MB.
	const maximumAllocation = 8 * 1024 * 1024
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > maximumAllocation {
		t.Fatalf("decoding %d bytes allocated %d bytes", len(input), allocated)
	}
}
