package outputruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type dispositionPlatform struct {
	outputcap.Platform
	disposition outputcap.RootOpenDisposition
}

func (platform dispositionPlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return platform.disposition
}

func newDirectoryAdmissionSecret(random io.Reader) ([sha256.Size]byte, error) {
	var secret [sha256.Size]byte
	if random == nil {
		return secret, transfer.ErrInvalidOutputBinding
	}
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return [sha256.Size]byte{}, err
	}
	if secret == [sha256.Size]byte{} {
		return [sha256.Size]byte{}, transfer.ErrInvalidOutputBinding
	}
	return secret, nil
}

func runtimeRootDisposition(disposition outputcap.RootOpenDisposition) FilesystemOutputRootDisposition {
	switch disposition {
	case outputcap.AuthorityCreatedRoot:
		return FilesystemOutputAuthorityCreatedRoot
	case outputcap.CallerProvidedContainer:
		return FilesystemOutputCallerProvidedContainer
	default:
		return ""
	}
}

func runtimeOutputError(
	ctx context.Context,
	code transferfault.OutputCode,
	operation string,
	cause error,
) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	value, _ := transferfault.NewOutput(transferfault.ScopeOutputPause, code)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", operation, cause))
}

func runtimeDependencyError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return transferfault.Wrap(transferfault.DependencyContractFault(), fmt.Errorf("%s: %w", operation, cause))
}
