package terminalcanvas

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeTextNeutralizesTerminalAndBidiControls(t *testing.T) {
	unsafe := string([]byte{'o', 'k', 0x1b}) + "[31m\n\t\r\x00\x7f\u0080\u202e\u2028" + string([]byte{0xff}) + "é"
	want := `ok\x1b[31m\n\t\r\x00\x7f\u0080\u202e\u2028\xffé`

	got := EscapeText(unsafe)
	if got != want {
		t.Fatalf("EscapeText() = %q, want %q", got, want)
	}
	for _, forbidden := range []rune{'\x1b', '\n', '\r', '\t', '\u0080', '\u202e', '\u2028'} {
		if strings.ContainsRune(got, forbidden) {
			t.Fatalf("escaped output retained control %U in %q", forbidden, got)
		}
	}
	if !utf8.ValidString(got) {
		t.Fatalf("escaped output is not valid UTF-8: %q", got)
	}
}

func TestGraphemeCellWidthVectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "combining", text: "e\u0301", want: 1},
		{name: "wide", text: "界", want: 2},
		{name: "emoji", text: "👨‍👩‍👧‍👦", want: 2},
		{name: "escaped control", text: "\n", want: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := DisplayCells(test.text, nil); got != test.want {
				t.Fatalf("DisplayCells(%q) = %d, want %d", test.text, got, test.want)
			}
		})
	}
}

func TestLineOwnsSpanStorageAndMeasuresEscapedText(t *testing.T) {
	spans := []Span{{Text: "界"}, {Text: "\x1b"}}
	line := NewLine(spans...)
	spans[0].Text = "changed"

	if got, want := line.DisplayCells(nil), 6; got != want {
		t.Fatalf("DisplayCells() = %d, want %d", got, want)
	}
	returned := line.Spans()
	returned[0].Text = "also changed"
	if got := line.Spans()[0].Text; got != "界" {
		t.Fatalf("line storage was mutable through Spans(): %q", got)
	}
}

func TestLineMeasuresGraphemesAcrossStyleBoundaries(t *testing.T) {
	line := NewLine(
		Span{Text: "e", Style: StyleStrong},
		Span{Text: "\u0301", Style: StyleAccent},
	)
	if got := line.DisplayCells(nil); got != 1 {
		t.Fatalf("DisplayCells() = %d, want one cross-span grapheme cell", got)
	}
}
