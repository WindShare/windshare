package osfs

import (
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

var (
	errUnsupportedOutputVolume = outputfault.ErrUnsupportedVolume
	errOutputRootUnsafe        = outputfault.ErrRootUnsafe
	errOutputIntentUnsafe      = outputfault.ErrIntentUnsafe
	errOutputSessionActive     = outputfault.ErrSessionActive
	errOutputSessionClosed     = outputfault.ErrSessionClosed
	errOutputFileActive        = outputfault.ErrFileActive
	errOutputTransactionLimit  = outputfault.ErrTransactionLimit
	errOutputInspectionLimit   = outputfault.ErrInspectionLimit
	errLegacyOutputState       = outputfault.ErrLegacyState
	errReservedOutputPath      = outputfault.ErrReservedPath
)

func outputFault(scope transfer.OutputFaultScope, code transfer.OutputFaultCode, cause error) error {
	return outputfault.New(scope, code, cause)
}
