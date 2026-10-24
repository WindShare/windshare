package recovery

import (
	"context"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/output"
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
	reasonDurability               = "output-durability-failed"
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
	Stage output.FailureStage
}

func (detail FailureDetail) Valid() bool {
	return detail == (FailureDetail{}) || detail.Stage.Valid()
}

var ErrBusy = output.ErrBusy

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
	case errors.Is(err, ErrBusy):
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

// AuthorityCloseFailure identifies release failures independently of whether
// the requested discard itself reached a terminal state.
func AuthorityCloseFailure(err error) error {
	return output.FailureForStage(err, output.FailureClose)
}
func failureDiagnostic(err error, fallback string) (string, FailureDetail) {
	diagnostic, ok := output.FindDiagnostic(err)
	if !ok {
		return fallback, FailureDetail{}
	}
	reason := fallback
	switch diagnostic.Stage {
	case output.FailureDestinationBinding:
		reason = reasonDestinationBinding
	case output.FailureInventory:
		reason = reasonInventoryPaging
	case output.FailureLookup:
		reason = reasonActiveLookup
	case output.FailureOpen:
		reason = reasonOperationAcquisition
	case output.FailureCreate:
		reason = reasonOperationAdmission
	case output.FailureCheckpoint:
		reason = reasonCheckpointReconciliation
	case output.FailureDurability:
		reason = reasonDurability
	case output.FailureClose:
		reason = reasonAuthorityClose
	}
	return reason, FailureDetail{Stage: diagnostic.Stage}
}
