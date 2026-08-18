package terminalcanvas

// Style describes presentation intent without admitting terminal control bytes.
// The canvas remains the only component that translates intent into ANSI.
type Style uint8

const (
	StyleDefault Style = iota
	StyleStrong
	StyleMuted
	StyleAccent
	StyleSuccess
	StyleWarning
	StyleError
)

// Span is semantic terminal text. Text may be untrusted; Canvas escapes it before
// measuring or writing it.
type Span struct {
	Text  string
	Style Style
}

// Line is a complete semantic line without a trailing newline.
//
// Its spans are private so a caller cannot mutate an active progress line while
// another goroutine is redrawing it.
type Line struct {
	spans []Span
}

// NewLine copies spans into an immutable semantic line.
func NewLine(spans ...Span) Line {
	return Line{spans: append([]Span(nil), spans...)}
}

// Plain constructs an unstyled semantic line.
func Plain(text string) Line {
	return NewLine(Span{Text: text})
}

// Spans returns a copy suitable for renderer-side inspection or composition.
func (line Line) Spans() []Span {
	return append([]Span(nil), line.spans...)
}

func (line Line) clone() Line {
	return NewLine(line.spans...)
}
