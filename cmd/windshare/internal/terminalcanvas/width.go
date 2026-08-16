package terminalcanvas

import (
	"strings"

	"github.com/rivo/uniseg"
)

// CellWidthFunc reports terminal display cells, not bytes or runes.
type CellWidthFunc func(string) int

// GraphemeCellWidth follows terminal grapheme and East Asian/emoji width rules.
func GraphemeCellWidth(text string) int {
	return uniseg.StringWidth(text)
}

// DisplayCells escapes text before measuring it, matching Canvas output.
func DisplayCells(text string, cellWidth CellWidthFunc) int {
	return measuredCells(EscapeText(text), normalizeCellWidth(cellWidth))
}

// DisplayCells reports the width of the line exactly as Canvas will display it.
func (line Line) DisplayCells(cellWidth CellWidthFunc) int {
	_, text := escapedSpans(line)
	return measuredCells(text, normalizeCellWidth(cellWidth))
}

func normalizeCellWidth(cellWidth CellWidthFunc) CellWidthFunc {
	if cellWidth == nil {
		return GraphemeCellWidth
	}
	return cellWidth
}

func measuredCells(text string, cellWidth CellWidthFunc) int {
	cells := cellWidth(text)
	if cells < 0 {
		return 0
	}
	return cells
}

func clipSpans(line Line, columns int, cellWidth CellWidthFunc) []Span {
	escaped, text := escapedSpans(line)
	if columns <= 0 {
		return escaped
	}

	remaining := columns
	cutoff := 0
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		clusterCells := measuredCells(graphemes.Str(), cellWidth)
		if clusterCells > remaining {
			break
		}
		remaining -= clusterCells
		_, cutoff = graphemes.Positions()
		if remaining == 0 {
			break
		}
	}
	if cutoff == len(text) {
		return escaped
	}

	clipped := make([]Span, 0, len(escaped))
	consumed := 0
	for _, span := range escaped {
		spanEnd := consumed + len(span.Text)
		switch {
		case cutoff >= spanEnd:
			clipped = append(clipped, span)
		case cutoff > consumed:
			clipped = append(clipped, Span{Text: span.Text[:cutoff-consumed], Style: span.Style})
			return clipped
		default:
			return clipped
		}
		consumed = spanEnd
	}
	return clipped
}

func escapedSpans(line Line) ([]Span, string) {
	escaped := make([]Span, 0, len(line.spans))
	var text strings.Builder
	for _, semantic := range line.spans {
		safeText := EscapeText(semantic.Text)
		if safeText == "" {
			continue
		}
		escaped = append(escaped, Span{Text: safeText, Style: semantic.Style})
		text.WriteString(safeText)
	}
	return escaped, text.String()
}
