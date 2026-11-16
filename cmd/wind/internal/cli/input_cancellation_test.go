package cli

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
)

func TestReceiveKeyWaitCancelsWithoutInput(t *testing.T) {
	for _, command := range []receiveCommand{receiveGet, receiveResumeRetry} {
		t.Run(string(command), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				capability := newSemanticCapability(t)
				bare, _, err := capability.SplitURL("https://relay.example")
				if err != nil {
					t.Fatal(err)
				}
				input, writer := io.Pipe()
				defer input.Close()
				defer writer.Close()
				app, _, _ := newSemanticTestApp(input)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan requestParseOutcome, 1)
				go func() {
					_, outcome := app.parseReceiveRequest(ctx, command, []string{bare})
					done <- outcome
				}()
				synctest.Wait()
				cancel()
				outcome := <-done
				if outcome != requestParseCancelled || outcome.exitCode() != ExitFailure {
					t.Fatalf("cancelled key prompt outcome=%v exit=%d", outcome, outcome.exitCode())
				}
			})
		})
	}
}
