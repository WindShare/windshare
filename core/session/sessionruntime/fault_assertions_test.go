package sessionruntime

import (
	"errors"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func boundaryFaultForTest(err error) (transferfault.Fault, bool) {
	var boundary *transferfault.BoundaryError
	if !errors.As(err, &boundary) || boundary == nil || !boundary.Fault().Valid() {
		return transferfault.Fault{}, false
	}
	return boundary.Fault(), true
}

func isSessionTerminalBoundaryForTest(err error) bool {
	value, ok := boundaryFaultForTest(err)
	return ok && value.Domain() == transferfault.DomainSession &&
		value.Scope() == transferfault.ScopeSessionTerminal
}
