package recovery

import (
	"context"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/engine/internal/task"
)

const (
	reasonDestinationUnverified    = "destination-or-registry-unverified"
	reasonCancelled                = "command-cancelled"
	reasonDestinationBusy          = "destination-already-in-use"
	reasonOperationRunning         = "operation-already-running"
	reasonOperationChanged         = "operation-no-longer-matches"
	reasonDestinationBinding       = "destination-binding-failed"
	reasonInventoryPaging          = "inventory-paging-failed"
	reasonActiveLookup             = "active-lookup-failed"
	reasonOperationAcquisition     = "operation-acquisition-failed"
	reasonOperationAdmission       = "operation-admission-failed"
	reasonCheckpointReconciliation = "checkpoint-reconciliation-failed"
	reasonNativeDurability         = "native-durability-failed"
	reasonAuthorityClose           = "authority-close-failed"
)

type FailureKind uint8

const (
	FailureNeedsAttention FailureKind = iota + 1
	FailureBusy
	FailureChanged
	FailureCancelled
)

type FailureDetail struct {
	Stage          osfs.FilesystemOutputFailureStage
	Reconciliation osfs.FilesystemCheckpointReconciliationStep
	NativeClass    osfs.FilesystemNativeErrorClass
}

func (detail FailureDetail) Valid() bool {
	if detail == (FailureDetail{}) {
		return true
	}
	if !detail.Stage.Valid() {
		return false
	}
	if detail.Reconciliation != 0 {
		if !detail.Reconciliation.Valid() ||
			detail.Stage != osfs.FilesystemOutputFailureCheckpointReconciliation &&
				detail.Stage != osfs.FilesystemOutputFailureNativeDurability {
			return false
		}
	}
	return detail.NativeClass == 0 || detail.NativeClass.Valid()
}

type Failure struct {
	Kind         FailureKind
	Reason       string
	Detail       FailureDetail
	Cause        error
	Observations <-chan task.Observation
}

func (failure *Failure) Error() string {
	if failure == nil {
		return ""
	}
	return failure.Reason
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

// DestinationFailure classifies authority acquisition separately from an
// operation race: the same lock failure needs a different user action.
func DestinationFailure(err error) *Failure {
	return classifyFailure(err, false)
}

func classifyFailure(err error, operation bool) *Failure {
	failure := &Failure{Kind: FailureNeedsAttention, Reason: reasonDestinationUnverified, Cause: err}
	if operation {
		failure.Reason = string(AttentionOperation)
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, task.ErrCancelled), errors.Is(err, task.ErrApplicationClosed):
		failure.Kind, failure.Reason = FailureCancelled, reasonCancelled
	case errors.Is(err, osfs.ErrResumeStateBusy):
		failure.Kind, failure.Reason = FailureBusy, reasonDestinationBusy
		if operation {
			failure.Reason = reasonOperationRunning
		}
	case operation && errors.Is(err, fs.ErrNotExist):
		failure.Kind, failure.Reason = FailureChanged, reasonOperationChanged
	default:
		failure.Reason, failure.Detail = failureDiagnostic(err, failure.Reason)
	}
	return failure
}

func failureDiagnostic(err error, fallback string) (string, FailureDetail) {
	diagnostic, ok := osfs.FilesystemOutputDiagnosticFor(err)
	if !ok || !diagnostic.Valid() {
		return fallback, FailureDetail{}
	}
	detail := FailureDetail{
		Stage: diagnostic.Stage, Reconciliation: diagnostic.ReconciliationStep,
		NativeClass: diagnostic.NativeErrorClass,
	}
	switch diagnostic.Stage {
	case osfs.FilesystemOutputFailureDestinationBinding:
		return reasonDestinationBinding, detail
	case osfs.FilesystemOutputFailureInventoryPaging:
		return reasonInventoryPaging, detail
	case osfs.FilesystemOutputFailureActiveLookup:
		return reasonActiveLookup, detail
	case osfs.FilesystemOutputFailureOperationAcquisition:
		return reasonOperationAcquisition, detail
	case osfs.FilesystemOutputFailureOperationAdmission:
		return reasonOperationAdmission, detail
	case osfs.FilesystemOutputFailureCheckpointReconciliation:
		return reasonCheckpointReconciliation, detail
	case osfs.FilesystemOutputFailureNativeDurability:
		return reasonNativeDurability, detail
	case osfs.FilesystemOutputFailureAuthorityClose:
		return reasonAuthorityClose, detail
	default:
		return fallback, FailureDetail{}
	}
}

// AuthorityCloseFailure identifies release failures independently of whether
// the requested discard itself reached a terminal state.
func AuthorityCloseFailure(err error) error {
	return osfs.FilesystemOutputFailureForStage(err, osfs.FilesystemOutputFailureAuthorityClose)
}
