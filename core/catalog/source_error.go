package catalog

import "fmt"

// SourceFailure distinguishes adapter failures without requiring native error
// types or leaking the provider's lookup interpretation into its consumers.
type SourceFailure uint8

const (
	SourceFailureUnavailable SourceFailure = iota + 1
	SourceFailureMissing
	SourceFailureAccessDenied
	SourceFailureUnsupported
	SourceFailureStale
)

type SourceError struct {
	Operation string
	Reference SourceReference
	Failure   SourceFailure
	Cause     error
}

func (err *SourceError) Error() string {
	return fmt.Sprintf("file source %s: %v", err.Operation, err.Cause)
}

func (err *SourceError) Unwrap() error { return err.Cause }

// SelectionError identifies the caller's selected item when acquisition fails.
// Label is display-only: providers retain all authority to resolve the selection.
type SelectionError struct {
	Label string
	Cause error
}

func (err *SelectionError) Error() string {
	return fmt.Sprintf("select source %q: %v", err.Label, err.Cause)
}
func (err *SelectionError) Unwrap() error { return err.Cause }
