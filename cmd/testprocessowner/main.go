// Command testprocessowner isolates correctness-test descendants from their
// runner so deadlines and runner loss still retire the complete process tree.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/windshare/windshare/internal/processowner"
)

const (
	commandSelfCheck = "self-check"
	commandSupervise = "supervise"
	selfCheckOutput  = "testprocessowner ready\n"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, input io.Reader, output io.Writer) error {
	if len(arguments) == 1 && arguments[0] == commandSelfCheck {
		_, err := io.WriteString(output, selfCheckOutput)
		return err
	}
	if len(arguments) == 0 || arguments[0] != commandSupervise {
		return errors.New("testprocessowner requires self-check or supervise")
	}
	config, err := processowner.DecodeConfig(input)
	if err != nil {
		return fmt.Errorf("decode process configuration: %w", err)
	}
	return runPlatform(arguments[1:], config)
}
