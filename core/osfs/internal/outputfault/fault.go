package outputfault

import (
	"errors"

	"github.com/windshare/windshare/core/transfer"
)

var (
	ErrUnsupportedVolume       = errors.New("osfs: output root is not on a certified filesystem")
	ErrRootUnsafe              = errors.New("osfs: output root recovery metadata is unsafe")
	ErrIntentUnsafe            = errors.New("osfs: resume-intent namespace is unsafe")
	ErrSessionActive           = errors.New("osfs: output session is already active")
	ErrSessionClosed           = errors.New("osfs: output session is closed")
	ErrFileActive              = errors.New("osfs: output file transaction is already active")
	ErrTransactionLimit        = errors.New("osfs: output transaction limit reached")
	ErrInspectionLimit         = errors.New("osfs: output namespace inspection limit reached")
	ErrLegacyState             = errors.New("osfs: legacy v2 output state is untrusted")
	ErrReservedPath            = errors.New("osfs: selected output path collides with private output state")
	ErrOutOfRange              = errors.New("osfs: byte range is out of bounds")
	ErrPathEscape              = errors.New("osfs: path escapes the output root")
	ErrAncestryAuthorityDenied = errors.New("osfs: output ancestry authority denied")
)

func New(scope transfer.OutputFaultScope, code transfer.OutputFaultCode, cause error) error {
	return transfer.NewOutputFault(scope, code, cause)
}
