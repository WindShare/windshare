//go:build !linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	unsupportedSelfCheckCommand = "self-check"
	unsupportedPlatformMessage  = "linux process owner is unsupported on this platform"
)

func main() {
	runUnsupportedPlatform(os.Args[1:], os.Stderr, os.Exit)
}

func runUnsupportedPlatform(arguments []string, diagnostic io.Writer, terminate func(int)) {
	if len(arguments) == 1 && arguments[0] == unsupportedSelfCheckCommand {
		_, _ = fmt.Fprintln(diagnostic, unsupportedPlatformMessage)
		terminate(1)
		// The real terminator cannot return. This boundary deliberately can so the
		// unsupported-platform contract remains testable without killing the suite.
		return
	}
	panic(errors.New(unsupportedPlatformMessage))
}
