package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
)

func TestResumeHelpPreservesStdoutAndUsesCanvasStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
	if code := app.runResume(context.Background(), []string{"help"}); code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "windshare resume list") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTerminalOutputUsesCanvasForCompleteEscapedLines(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{
		Stderr: &stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{}
		}),
	}
	writer := app.stderrWriter()
	if written, err := writer.Write([]byte("first\r\nsecond\x1b\n")); err != nil || written != len("first\r\nsecond\x1b\n") {
		t.Fatalf("Write=(%d,%v)", written, err)
	}
	if got := stderr.String(); got != "first\nsecond\\x1b\n" {
		t.Fatalf("stderr=%q", got)
	}
}

func TestTerminalWriterFailureNeverBecomesDiagnosticAuthority(t *testing.T) {
	want := errors.New("stderr unavailable")
	app := &App{Stderr: errorWriter{err: want}}
	payload := []byte("diagnostic\n")
	if written, err := app.stderrWriter().Write(payload); err != nil || written != len(payload) {
		t.Fatalf("Write=(%d,%v)", written, err)
	}
	if err := app.terminalOutput().canvas.Err(); !errors.Is(err, want) {
		t.Fatalf("Canvas.Err=%v want=%v", err, want)
	}
	app.writeCompleteLine("later %s", strings.Repeat("x", 4))
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }
