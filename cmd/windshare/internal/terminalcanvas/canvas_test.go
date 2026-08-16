package terminalcanvas

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestCanvasClearInsertRedrawAndFinishOrdering(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{
		Writer:       &output,
		Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true, Columns: 80}),
	})

	canvas.ReplaceProgress(Plain("progress"))
	canvas.Insert([]Line{Plain("warning")})
	canvas.ReplaceProgress(Plain("next"))
	canvas.FinishProgress()
	canvas.Close()

	want := clearTerminalLine + "progress" +
		clearTerminalLine + "warning\n" + clearTerminalLine + "progress" +
		clearTerminalLine + "next" +
		clearTerminalLine + "\n"
	if got := output.String(); got != want {
		t.Fatalf("output bytes:\n got %q\nwant %q", got, want)
	}
	if err := canvas.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
}

func TestCanvasTerminalCapabilitiesRemainIndependent(t *testing.T) {
	tests := []struct {
		name         string
		capabilities Capabilities
		want         string
	}{
		{
			name:         "ansi cursor without color",
			capabilities: Capabilities{Interactive: true, ANSI: true, Color: false},
			want:         clearTerminalLine + "warning",
		},
		{
			name:         "color with cursor ansi",
			capabilities: Capabilities{Interactive: true, ANSI: true, Color: true},
			want:         clearTerminalLine + "\x1b[33mwarning\x1b[0m",
		},
		{
			name:         "interactive without ansi is plain",
			capabilities: Capabilities{Interactive: true, ANSI: false, Color: true},
			want:         "warning\n",
		},
		{
			name:         "redirected disables ansi despite flags",
			capabilities: Capabilities{Interactive: false, ANSI: true, Color: true},
			want:         "warning\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			canvas := New(Config{Writer: &output, Capabilities: fixedProvider(test.capabilities)})
			canvas.ReplaceProgress(NewLine(Span{Text: "warning", Style: StyleWarning}))
			if got := output.String(); got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCanvasOwnsEveryStyleSequence(t *testing.T) {
	styles := []struct {
		style Style
		ansi  string
	}{
		{StyleDefault, ""},
		{StyleStrong, "\x1b[1m"},
		{StyleMuted, "\x1b[2m"},
		{StyleAccent, "\x1b[36m"},
		{StyleSuccess, "\x1b[32m"},
		{StyleWarning, "\x1b[33m"},
		{StyleError, "\x1b[31m"},
		{Style(255), ""},
	}
	for _, test := range styles {
		var output bytes.Buffer
		canvas := New(Config{
			Writer:       &output,
			Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true, Color: true}),
		})
		canvas.Insert([]Line{NewLine(Span{Text: "text", Style: test.style})})
		want := "text\n"
		if test.ansi != "" {
			want = test.ansi + "text" + resetStyle + "\n"
		}
		if got := output.String(); got != want {
			t.Errorf("style %d output = %q, want %q", test.style, got, want)
		}
	}
}

func TestCanvasSamplesWidthForEveryRender(t *testing.T) {
	var output bytes.Buffer
	columns := 3
	canvas := New(Config{
		Writer: &output,
		Capabilities: CapabilityProviderFunc(func() Capabilities {
			return Capabilities{Interactive: true, ANSI: true, Columns: columns}
		}),
	})
	progress := Plain("界abc")

	canvas.ReplaceProgress(progress)
	columns = 2
	canvas.Insert([]Line{Plain("log")})

	want := clearTerminalLine + "界a" + clearTerminalLine + "lo\n" + clearTerminalLine + "界"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestCanvasClipsWholeGraphemeAcrossStyledSpans(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{
		Writer:       &output,
		Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true, Columns: 1}),
	})
	canvas.ReplaceProgress(NewLine(
		Span{Text: "e", Style: StyleStrong},
		Span{Text: "\u0301x", Style: StyleAccent},
	))

	if got, want := output.String(), clearTerminalLine+"e\u0301"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestCanvasUsesInjectedCellWidth(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{
		Writer:       &output,
		Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true, Columns: 2}),
		CellWidth: func(text string) int {
			return len([]rune(text))
		},
	})
	canvas.ReplaceProgress(Plain("界ab"))
	if got, want := output.String(), clearTerminalLine+"界a"; got != want {
		t.Fatalf("output = %q, want injected-width clipping %q", got, want)
	}
}

func TestCanvasEscapesBeforeClippingAndStyling(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{
		Writer:       &output,
		Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true, Color: true, Columns: 10}),
	})
	canvas.Insert([]Line{NewLine(Span{Text: "x\x1b[2J\ny\u202e", Style: StyleError})})

	want := "\x1b[31mx\\x1b[2J\\n\x1b[0m\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	semantic := strings.ReplaceAll(output.String(), "\x1b[31m", "")
	semantic = strings.ReplaceAll(semantic, "\x1b[0m", "")
	if strings.ContainsRune(semantic, '\x1b') {
		t.Fatalf("semantic output contains injected ESC: %q", semantic)
	}
}

func TestCanvasPlainModeUsesOnlyCompleteLines(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{Writer: &output, Capabilities: fixedProvider(Capabilities{Interactive: true})})

	canvas.ReplaceProgress(Plain("first"))
	canvas.ReplaceProgress(Plain("second"))
	canvas.Insert([]Line{Plain("log")})
	canvas.FinishProgress()
	canvas.Close()

	if got, want := output.String(), "first\nsecond\nlog\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if strings.ContainsAny(output.String(), "\r\x1b") {
		t.Fatalf("plain output contained terminal control: %q", output.String())
	}
}

func TestCanvasTerminatesDynamicLineWhenANSIBecomesUnavailable(t *testing.T) {
	var output bytes.Buffer
	dynamic := true
	canvas := New(Config{
		Writer: &output,
		Capabilities: CapabilityProviderFunc(func() Capabilities {
			return Capabilities{Interactive: true, ANSI: dynamic}
		}),
	})

	canvas.ReplaceProgress(Plain("progress"))
	dynamic = false
	canvas.Insert([]Line{Plain("plain")})
	canvas.Close()

	want := clearTerminalLine + "progress\nplain\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestCanvasLatchesShortWriteAndStopsOutput(t *testing.T) {
	writer := &shortWriter{}
	canvas := New(Config{Writer: writer})

	canvas.Insert([]Line{Plain("first")})
	canvas.Insert([]Line{Plain("second")})
	canvas.Close()

	if !errors.Is(canvas.Err(), io.ErrShortWrite) {
		t.Fatalf("Err() = %v, want io.ErrShortWrite", canvas.Err())
	}
	if writer.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", writer.calls)
	}
}

func TestCanvasLatchesFirstWriterError(t *testing.T) {
	first := errors.New("first")
	writer := &errorWriter{err: first}
	canvas := New(Config{Writer: writer})

	canvas.Insert([]Line{Plain("first")})
	writer.err = errors.New("second")
	canvas.ReplaceProgress(Plain("second"))

	if got := canvas.Err(); !errors.Is(got, first) {
		t.Fatalf("Err() = %v, want first error", got)
	}
	if writer.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", writer.calls)
	}
}

func TestCanvasCloseAddsExactlyOneNewlineToDynamicProgress(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{
		Writer:       &output,
		Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true}),
	})

	canvas.ReplaceProgress(Plain("progress"))
	canvas.Close()
	canvas.Close()
	canvas.Insert([]Line{Plain("ignored")})

	if got := strings.Count(output.String(), "\n"); got != 1 {
		t.Fatalf("newline count = %d in %q, want 1", got, output.String())
	}
	if !strings.HasSuffix(output.String(), "\n") {
		t.Fatalf("output did not end in newline: %q", output.String())
	}
}

func TestCanvasConcurrentProducersAreSerialized(t *testing.T) {
	var output bytes.Buffer
	canvas := New(Config{
		Writer:       &output,
		Capabilities: fixedProvider(Capabilities{Interactive: true, ANSI: true, Columns: 40}),
	})

	var producers sync.WaitGroup
	for index := range 40 {
		producers.Add(1)
		go func(index int) {
			defer producers.Done()
			canvas.ReplaceProgress(Plain("progress"))
			canvas.Insert([]Line{Plain("log")})
		}(index)
	}
	producers.Wait()
	canvas.Close()

	if err := canvas.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if !strings.HasSuffix(output.String(), "\n") {
		t.Fatalf("closed dynamic output lacks final newline")
	}
}

func TestCanvasWithoutWriterStartsDisabled(t *testing.T) {
	canvas := New(Config{})
	canvas.Insert([]Line{Plain("ignored")})
	if !errors.Is(canvas.Err(), ErrWriterUnavailable) {
		t.Fatalf("Err() = %v, want ErrWriterUnavailable", canvas.Err())
	}
}

type shortWriter struct {
	calls int
}

func (writer *shortWriter) Write(payload []byte) (int, error) {
	writer.calls++
	if len(payload) == 0 {
		return 0, nil
	}
	return len(payload) - 1, nil
}

type errorWriter struct {
	calls int
	err   error
}

func (writer *errorWriter) Write([]byte) (int, error) {
	writer.calls++
	return 0, writer.err
}

func fixedProvider(capabilities Capabilities) CapabilityProvider {
	return CapabilityProviderFunc(func() Capabilities { return capabilities })
}
