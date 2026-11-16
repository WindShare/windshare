package resumecommand

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
)

func TestDiscardConfirmationWaitCancelsWithoutInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input, writer := io.Pipe()
		defer input.Close()
		defer writer.Close()
		terminal := stdioResumeConfirmationTerminal{
			input: input, output: testDiagnosticOutput{writer: io.Discard}, interactive: true,
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			line, err := terminal.ReadLine(ctx, "confirm")
			if line != "" {
				t.Errorf("cancelled confirmation returned %q", line)
			}
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("confirmation error=%v", err)
		}
	})
}
