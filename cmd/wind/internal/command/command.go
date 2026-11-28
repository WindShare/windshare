// Package command provides the shared identity, exit-code contract, and
// interleaved flag-parsing primitives used by wind and its subcommands.
package command

import "fmt"

// Name stays independent of os.Args[0] so help and diagnostics remain stable
// when callers invoke the executable through an absolute path or a symlink.
const Name = "wind"

// ExitCode defines the process exit status contract shared across all wind
// commands so scripts can distinguish retryable network errors, user input
// errors, and source drift.
type ExitCode int

const (
	ExitOK      ExitCode = 0
	ExitFailure ExitCode = 1 // Runtime failure: interrupted transfer, local IO, internal error.
	ExitUsage   ExitCode = 2 // User error: arguments, capability/key, selection, manifest limit.
	ExitNetwork ExitCode = 3 // Network or relay unreachable or denied.
	ExitDrift   ExitCode = 4 // Source modified during share.
)

// Int returns the OS process exit code.
func (code ExitCode) Int() int {
	return int(code)
}

// LineOutput commits complete diagnostic records immediately; it is not a byte stream.
type LineOutput interface {
	WriteLines(...string)
}

// WriteLines writes records to output when non-nil.
func WriteLines(output LineOutput, records ...string) {
	if output != nil {
		output.WriteLines(records...)
	}
}

// WriteLinef formats one diagnostic record and writes it to output when non-nil.
func WriteLinef(output LineOutput, format string, args ...any) {
	if output != nil {
		output.WriteLines(fmt.Sprintf(format, args...))
	}
}

// IsHelpFlag reports whether token is a standard flag-style help spelling.
func IsHelpFlag(token string) bool {
	return token == "-h" || token == "-help" || token == "--help"
}

// IsHelpToken reports whether token is a help verb or flag spelling.
func IsHelpToken(token string) bool {
	return token == "help" || IsHelpFlag(token)
}
