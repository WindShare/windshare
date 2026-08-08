package resumeauthority

import (
	"context"
	"errors"
	"fmt"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type RepositoryErrorCode string

const (
	RepositoryBusy              RepositoryErrorCode = "busy"
	RepositoryCorruptRecord     RepositoryErrorCode = "corrupt-record"
	RepositoryUnsafeInstall     RepositoryErrorCode = "unsafe-install"
	RepositoryOwnershipMismatch RepositoryErrorCode = "ownership-mismatch"
	RepositoryStateIO           RepositoryErrorCode = "state-io"
)

func (code RepositoryErrorCode) Valid() bool {
	switch code {
	case RepositoryBusy, RepositoryCorruptRecord, RepositoryUnsafeInstall,
		RepositoryOwnershipMismatch, RepositoryStateIO:
		return true
	default:
		return false
	}
}

var ErrBusy = errors.New("resume state authority is busy")

// RepositoryError is the adapter boundary. The cause is diagnostic only; Code
// is the sole input to normalized transfer fault policy.
type RepositoryError struct {
	code      RepositoryErrorCode
	operation string
	cause     error
}

func NewRepositoryError(code RepositoryErrorCode, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if !code.Valid() || operation == "" {
		return &repositoryProjectionError{cause: cause}
	}
	return &RepositoryError{code: code, operation: operation, cause: cause}
}

// repositoryProjectionError keeps the rejected cause diagnostic-only. An
// invalid adapter projection must not make the raw cause searchable authority.
type repositoryProjectionError struct {
	cause error
}

func (err *repositoryProjectionError) Error() string {
	if err == nil {
		return ErrInvalidContract.Error()
	}
	return fmt.Sprintf("%s: repository error projection: %v", ErrInvalidContract, err.cause)
}

func (err *repositoryProjectionError) Is(target error) bool {
	return err != nil && target == ErrInvalidContract
}

func (err *RepositoryError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("resume repository %s: %s: %v", err.operation, err.code, err.cause)
}

func (err *RepositoryError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func (err *RepositoryError) Code() RepositoryErrorCode {
	if err == nil {
		return ""
	}
	return err.code
}

func (err *RepositoryError) Operation() string {
	if err == nil {
		return ""
	}
	return err.operation
}

func (err *RepositoryError) Is(target error) bool {
	return target == ErrBusy && err != nil && err.code == RepositoryBusy
}

// NormalizeFaults receives distinct collaborator outcomes, not a pre-joined
// raw error graph. Each outcome is normalized once and the closed values are
// then joined deterministically.
func NormalizeFaults(ctx context.Context, outcomes ...error) (transferfault.Fault, bool) {
	if ctx != nil && ctx.Err() != nil {
		return transferfault.Fault{}, false
	}
	normalized := make([]transferfault.Fault, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome == nil {
			continue
		}
		if errors.Is(outcome, context.Canceled) || errors.Is(outcome, context.DeadlineExceeded) {
			return transferfault.Fault{}, false
		}
		normalized = append(normalized, normalizeFault(outcome))
	}
	return transferfault.Join(normalized...), true
}

func normalizeFault(outcome error) transferfault.Fault {
	var repositoryErr *RepositoryError
	if !errors.As(outcome, &repositoryErr) || repositoryErr == nil || !repositoryErr.code.Valid() {
		value, _ := transferfault.NewSession(
			transferfault.ScopeOutputPause,
			transferfault.SessionDependencyContract,
		)
		return value
	}
	code := transferfault.CheckpointStateIO
	switch repositoryErr.code {
	case RepositoryBusy:
		code = transferfault.CheckpointBusy
	case RepositoryCorruptRecord:
		code = transferfault.CheckpointCorruptRecord
	case RepositoryUnsafeInstall:
		code = transferfault.CheckpointUnsafeInstall
	case RepositoryOwnershipMismatch:
		code = transferfault.CheckpointOwnershipMismatch
	case RepositoryStateIO:
		code = transferfault.CheckpointStateIO
	}
	value, _ := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, code)
	return value
}
