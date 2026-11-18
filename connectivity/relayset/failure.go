package relayset

import (
	"fmt"
	"strings"
)

// RegistrationFailure retains endpoint identity across the aggregate readiness
// result, so applications can recover the rejected authority without parsing text.
type RegistrationFailure struct {
	URL   string
	Cause error
}

func (failure *RegistrationFailure) Error() string {
	address, _, _ := strings.Cut(failure.URL, "?")
	return fmt.Sprintf("register %s: %v", address, failure.Cause)
}

func (failure *RegistrationFailure) Unwrap() error { return failure.Cause }
