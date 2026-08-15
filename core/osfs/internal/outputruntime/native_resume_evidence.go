package outputruntime

import (
	"context"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
)

func nativeResumeCleanupFailure(
	err error,
) (resumeauthority.CleanupState, error) {
	if err == nil {
		return resumeauthority.CleanupPending, nil
	}
	if nativeResumeUncertain(err) {
		return resumeauthority.CleanupPending, nil
	}
	return resumeauthority.CleanupPending, nativeResumeError(err)
}

func nativeResumeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, resumeauthority.ErrBusy) {
		return err
	}
	var checkpointErr *checkpointstore.Error
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
		errors.As(err, &checkpointErr) && checkpointErr != nil &&
			checkpointErr.Code() == checkpointstore.ErrorBusy {
		return errors.Join(resumeauthority.ErrBusy, err)
	}
	return err
}

func nativeResumeUncertain(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, resumeauthority.ErrBusy) {
		return false
	}
	if errors.Is(err, ErrNativeResumeOwnershipUnknown) ||
		errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, outputcap.ErrUnsafeNamespace) ||
		errors.Is(err, outputcap.ErrNamespaceCollision) ||
		errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) ||
		errors.Is(err, destinationauthority.ErrRetainedRootChanged) ||
		errors.Is(err, destinationauthority.ErrControlNamespaceChanged) ||
		errors.Is(err, destinationauthority.ErrReservationCollision) ||
		errors.Is(err, destinationauthority.ErrReservationIndeterminate) ||
		errors.Is(err, directoryauthority.ErrRetainedAuthorityChanged) ||
		errors.Is(err, checkpointmodel.ErrInvalidOwnership) ||
		errors.Is(err, checkpointmodel.ErrOwnershipChecksum) ||
		errors.Is(err, checkpointmodel.ErrOwnershipNonCanonical) ||
		errors.Is(err, checkpointmodel.ErrInvalidRecord) ||
		errors.Is(err, checkpointmodel.ErrRecordChecksum) ||
		errors.Is(err, checkpointmodel.ErrRecordNonCanonical) ||
		errors.Is(err, checkpointmodel.ErrRecordBinding) ||
		errors.Is(err, checkpointmodel.ErrRecordGeneration) ||
		errors.Is(err, checkpointmodel.ErrRecordObjectConflict) ||
		errors.Is(err, checkpointmodel.ErrRecordRecovery) ||
		errors.Is(err, checkpointmodel.ErrRecordCrashBoundary) ||
		errors.Is(err, checkpointmodel.ErrInvalidOrdinaryOperation) ||
		errors.Is(err, checkpointmodel.ErrInvalidOrdinaryLifecycle) {
		return true
	}
	var checkpointErr *checkpointstore.Error
	if !errors.As(err, &checkpointErr) || checkpointErr == nil {
		return false
	}
	switch checkpointErr.Code() {
	case checkpointstore.ErrorCorruptRecord,
		checkpointstore.ErrorUnsafeInstall,
		checkpointstore.ErrorOwnershipMismatch:
		return true
	default:
		return false
	}
}
