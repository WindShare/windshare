package configcommand

import (
	"context"
	"io"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/engine/relayconfig"
)

func TestImportCancellationDoesNotWaitForOrSaveLateInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input, writer := io.Pipe()
		defer input.Close()
		defer writer.Close()
		saves := 0
		runner := Runner{
			Input: input,
			Save: func(relayconfig.Config) error {
				saves++
				return nil
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan command.ExitCode, 1)
		go func() { done <- runner.Run(ctx, []string{"import", "-"}) }()
		synctest.Wait()
		cancel()
		if result := <-done; result != command.ExitFailure {
			t.Fatalf("cancelled import result=%v", result)
		}
		if _, err := io.WriteString(writer, importedConfiguration); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if saves != 0 {
			t.Fatal("cancelled import saved late input")
		}
	})
}
