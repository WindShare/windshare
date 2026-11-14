package relayset

import "fmt"

// RegistrationFailure retains endpoint identity across the aggregate readiness
// result, so applications can recover the rejected authority without parsing text.
type RegistrationFailure struct {
	URL   string
	Cause error
}

func (failure *RegistrationFailure) Error() string {
	return fmt.Sprintf("register %s: %v", failure.URL, failure.Cause)
}

func (failure *RegistrationFailure) Unwrap() error { return failure.Cause }
