//go:build windows

package osfs

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func outputV3ControlSessionRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	fault, found := errors.AsType[*transfer.OutputFault](err)
	if !found || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %#v in %v, want scope=%v code=%v", fault, err, scope, code)
	}
}
