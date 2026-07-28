package osfs

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputFaultSentinelsProjectInternalIdentity(t *testing.T) {
	projections := []struct {
		root     error
		internal error
	}{
		{root: errUnsupportedOutputVolume, internal: outputfault.ErrUnsupportedVolume},
		{root: errOutputRootUnsafe, internal: outputfault.ErrRootUnsafe},
		{root: errOutputIntentUnsafe, internal: outputfault.ErrIntentUnsafe},
		{root: errOutputSessionActive, internal: outputfault.ErrSessionActive},
		{root: errOutputSessionClosed, internal: outputfault.ErrSessionClosed},
		{root: errOutputFileActive, internal: outputfault.ErrFileActive},
		{root: errOutputTransactionLimit, internal: outputfault.ErrTransactionLimit},
		{root: errOutputInspectionLimit, internal: outputfault.ErrInspectionLimit},
		{root: errLegacyOutputState, internal: outputfault.ErrLegacyState},
		{root: errReservedOutputPath, internal: outputfault.ErrReservedPath},
	}
	for _, projection := range projections {
		if projection.root != projection.internal {
			t.Fatalf("root sentinel does not preserve internal identity: %v", projection.root)
		}
	}

	failure := outputFault(transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe, errOutputIntentUnsafe)
	if !errors.Is(failure, errOutputIntentUnsafe) {
		t.Fatalf("root fault constructor lost projected cause: %v", failure)
	}
}
