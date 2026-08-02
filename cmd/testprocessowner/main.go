// Command testprocessowner is the sole external process-tree owner used by
// correctness tests and Browsergate on Windows and Linux.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	commandSelfCheck = "self-check"
	commandGuard     = "guard"
	commandSupervise = "supervise"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Target stdout and stderr are protocol surfaces while supervision runs.
		// Dispatch failures happen before a target exists and may be diagnosed here.
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	return runWithOutput(arguments, os.Stdout)
}

func runWithOutput(arguments []string, output io.Writer) error {
	if len(arguments) == 1 && arguments[0] == commandSelfCheck {
		_, err := fmt.Fprintf(
			output,
			"{\"schema_version\":%q,\"component\":\"testprocessowner\",\"milestone\":\"self_check\",\"outcome\":\"ready\"}\n",
			protocol.SelfCheckSchemaVersion,
		)
		return err
	}
	if len(arguments) == 0 {
		return errors.New("testprocessowner requires a command")
	}
	return runPlatform(arguments)
}
