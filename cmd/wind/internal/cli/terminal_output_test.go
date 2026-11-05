package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
)

func TestResumeHelpPreservesStdoutAndUsesCanvasStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
	if code := app.runResume(context.Background(), []string{"help"}); code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "wind resume list") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTerminalLineOutputCommitsRecordsWithoutFlush(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{
		Stderr: &stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{}
		}),
	}
	output := app.stderrLines()
	output.WriteLines()
	if stderr.Len() != 0 {
		t.Fatal("an empty record batch wrote output")
	}
	// A prompt has no newline to trigger a flush; an explicit empty record is
	// still a meaningful blank line. Neither depends on a command's wording.
	output.WriteLines("first\r\nsecond\x1b\n", "", "prompt")
	if got := stderr.String(); got != "first\nsecond\\x1b\n\nprompt\n" {
		t.Fatalf("stderr=%q", got)
	}
}

func TestTerminalLogHandlerPreservesJSONRecords(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	log := slog.New(app.stderrLines().jsonHandler()).With("operation_id", "record-boundary")
	messages := []string{"first\ncontinuation", "second"}
	for _, message := range messages {
		log.Info(message)
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != len(messages) {
		t.Fatalf("record count=%d want=%d", len(lines), len(messages))
	}
	for index, line := range lines {
		var record struct {
			Message     string `json:"msg"`
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Message != messages[index] || record.OperationID != "record-boundary" {
			t.Fatalf("record=%+v", record)
		}
	}
}

func TestTerminalOutputFailureNeverBecomesDiagnosticAuthority(t *testing.T) {
	want := errors.New("stderr unavailable")
	app := &App{Stderr: errorWriter{err: want}}
	app.stderrLines().WriteLines("diagnostic")
	if err := app.terminalOutput().canvas.Err(); !errors.Is(err, want) {
		t.Fatalf("Canvas.Err=%v want=%v", err, want)
	}
	app.writeCompleteLine("later %s", strings.Repeat("x", 4))
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }
