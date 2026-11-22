package filesystem

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/osfs"
)

type CheckpointReconciliationStep = osfs.CheckpointReconciliationStep

const (
	CheckpointCandidateObservation = osfs.CheckpointCandidateObservation
	CheckpointStageDurability      = osfs.CheckpointStageDurability
	CheckpointNamespaceDurability  = osfs.CheckpointNamespaceDurability
	CheckpointRecordPromotion      = osfs.CheckpointRecordPromotion
)

type NativeErrorClass = osfs.NativeErrorClass

const (
	NativeErrorAccessDenied     = osfs.NativeErrorAccessDenied
	NativeErrorSharingViolation = osfs.NativeErrorSharingViolation
	NativeErrorNotFound         = osfs.NativeErrorNotFound
	NativeErrorInvalidHandle    = osfs.NativeErrorInvalidHandle
	NativeErrorUnsupported      = osfs.NativeErrorUnsupported
	NativeErrorIO               = osfs.NativeErrorIO
	NativeErrorUnknown          = osfs.NativeErrorUnknown
)

// OutputFailureDiagnostic holds value-only native failure evidence for sealing
// or presentation at the filesystem adapter boundary.
type OutputFailureDiagnostic struct {
	Stage              output.FailureStage
	ReconciliationStep CheckpointReconciliationStep
	NativeErrorClass   NativeErrorClass
	FaultDomain        uint8
	NormalizedScope    uint8
	NormalizedCode     uint16
}

func (diagnostic OutputFailureDiagnostic) Valid() bool {
	if !diagnostic.Stage.Valid() {
		return false
	}
	if diagnostic.ReconciliationStep != 0 {
		if !diagnostic.ReconciliationStep.Valid() ||
			diagnostic.Stage != output.FailureCheckpoint &&
				diagnostic.Stage != output.FailureDurability {
			return false
		}
	}
	if diagnostic.NativeErrorClass != 0 && !diagnostic.NativeErrorClass.Valid() {
		return false
	}
	faultZero := diagnostic.FaultDomain == 0 &&
		diagnostic.NormalizedScope == 0 &&
		diagnostic.NormalizedCode == 0
	faultComplete := diagnostic.FaultDomain != 0 &&
		diagnostic.NormalizedScope != 0 &&
		diagnostic.NormalizedCode != 0
	return faultZero || faultComplete
}

type sealedNativeDetail struct {
	detail FailureDetail
}

func (value *sealedNativeDetail) Error() string {
	return fmt.Sprintf("filesystem output failed at %s", value.detail.Stage)
}

// SealOutputFailure seals value-only native evidence at the adapter boundary;
// engine consumers only observe the normalized application failure.
func SealOutputFailure(diagnostic OutputFailureDiagnostic) (error, bool) {
	if !diagnostic.Valid() {
		return nil, false
	}
	cause := &sealedNativeDetail{detail: FailureDetail{
		Stage:              diagnostic.Stage,
		ReconciliationStep: diagnostic.ReconciliationStep,
		NativeErrorClass:   diagnostic.NativeErrorClass,
	}}
	failure, _ := observation.ProjectNormalizedFault(
		diagnostic.FaultDomain, diagnostic.NormalizedScope, diagnostic.NormalizedCode,
	)
	return output.SealFailure(cause, output.FailureDiagnostic{
		Stage: diagnostic.Stage, Failure: failure,
	})
}

// FailureDetail retains provider reconciliation and OS error classes for host
// CLI rendering without making callers depend directly on osfs.
type FailureDetail struct {
	Stage              output.FailureStage
	ReconciliationStep CheckpointReconciliationStep
	NativeErrorClass   NativeErrorClass
}

func (detail FailureDetail) Valid() bool {
	return (detail.Stage == 0 || detail.Stage.Valid()) &&
		(detail.ReconciliationStep == 0 || detail.ReconciliationStep.Valid()) &&
		(detail.NativeErrorClass == 0 || detail.NativeErrorClass.Valid())
}

// FailureDetailFor extracts native reconciliation and OS error classification
// from a sealed output or recovery failure.
func FailureDetailFor(cause error) (FailureDetail, bool) {
	if diagnostic, ok := osfs.OutputDiagnosticFor(cause); ok {
		return FailureDetail{
			Stage:              projectFailureStage(diagnostic.Stage),
			ReconciliationStep: diagnostic.ReconciliationStep,
			NativeErrorClass:   diagnostic.NativeErrorClass,
		}, true
	}
	if sealed, ok := errors.AsType[*sealedNativeDetail](cause); ok && sealed != nil && sealed.detail.Valid() {
		return sealed.detail, true
	}
	return FailureDetail{}, false
}

func projectFailureStage(stage osfs.OutputFailureStage) output.FailureStage {
	switch stage {
	case osfs.OutputFailureDestinationBinding:
		return output.FailureDestinationBinding
	case osfs.OutputFailureInventory:
		return output.FailureInventory
	case osfs.OutputFailureLookup:
		return output.FailureLookup
	case osfs.OutputFailureOpen:
		return output.FailureOpen
	case osfs.OutputFailureCreate:
		return output.FailureCreate
	case osfs.OutputFailureCheckpoint:
		return output.FailureCheckpoint
	case osfs.OutputFailureDurability:
		return output.FailureDurability
	case osfs.OutputFailureClose:
		return output.FailureClose
	default:
		return 0
	}
}

func sealOutputError(cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, osfs.ErrOutputShareStopped) {
		return output.ErrShareStopped
	}
	if errors.Is(cause, osfs.ErrOutputBusy) && !errors.Is(cause, output.ErrBusy) {
		cause = errors.Join(output.ErrBusy, cause)
	}
	diagnostic, ok := osfs.OutputDiagnosticFor(cause)
	if !ok {
		return cause
	}
	stages := []osfs.OutputFailureStage{diagnostic.Stage}
	for stage := osfs.OutputFailureDestinationBinding; stage <= osfs.OutputFailureClose; stage++ {
		if stage != diagnostic.Stage {
			stages = append(stages, stage)
		}
	}
	var evidence []output.FailureEvidence
	for _, stage := range stages {
		matching := osfs.OutputFailureForStage(cause, stage)
		if matching == nil {
			continue
		}
		detail, found := osfs.OutputDiagnosticFor(matching)
		if !found {
			continue
		}
		failure, _ := observation.ProjectNormalizedFault(
			detail.FaultDomain, detail.NormalizedScope, detail.NormalizedCode,
		)
		evidence = append(evidence, output.FailureEvidence{
			Cause: matching,
			Diagnostic: output.FailureDiagnostic{
				Stage: projectFailureStage(stage), Failure: failure,
			},
		})
	}
	sealed, valid := output.SealFailures(cause, evidence)
	if !valid {
		return output.ErrContract
	}
	return sealed
}
