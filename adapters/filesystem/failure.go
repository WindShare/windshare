package filesystem

import (
	"errors"
	"fmt"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/osfs"
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
	Stage            output.FailureStage
	NativeErrorClass NativeErrorClass
	Fault            transferfault.Fault
}

func (diagnostic OutputFailureDiagnostic) Valid() bool {
	if !diagnostic.Stage.Valid() {
		return false
	}
	if diagnostic.NativeErrorClass != 0 && !diagnostic.NativeErrorClass.Valid() {
		return false
	}
	return diagnostic.Fault.IsZero() || diagnostic.Fault.Valid()
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
		Stage:            diagnostic.Stage,
		NativeErrorClass: diagnostic.NativeErrorClass,
	}}
	return output.SealFailure(cause, output.FailureDiagnostic{
		Stage: diagnostic.Stage, Fault: diagnostic.Fault,
	})
}

// FailureDetail retains provider OS error classes for host CLI rendering
// without making callers depend directly on osfs.
type FailureDetail struct {
	Stage            output.FailureStage
	NativeErrorClass NativeErrorClass
}

func (detail FailureDetail) Valid() bool {
	return (detail.Stage == 0 || detail.Stage.Valid()) &&
		(detail.NativeErrorClass == 0 || detail.NativeErrorClass.Valid())
}

// FailureDetailFor extracts native OS error classification from a sealed output
// or recovery failure.
func FailureDetailFor(cause error) (FailureDetail, bool) {
	if diagnostic, ok := osfs.OutputDiagnosticFor(cause); ok {
		return FailureDetail{
			Stage:            projectFailureStage(diagnostic.Stage),
			NativeErrorClass: diagnostic.NativeErrorClass,
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
	if errors.Is(cause, osfs.ErrOutputAlreadyRunning) {
		return output.ErrOperationAlreadyRunning
	}
	if errors.Is(cause, osfs.ErrOutputAmbiguous) {
		return output.ErrOperationAmbiguous
	}
	if attention, ok := errors.AsType[*osfs.OutputAttentionError](cause); ok && attention != nil {
		reason := output.AttentionReason(attention.Reason)
		if !reason.Valid() {
			reason = output.AttentionOperation
		}
		return &output.AttentionError{Reason: reason}
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
		evidence = append(evidence, output.FailureEvidence{
			Cause: matching,
			Diagnostic: output.FailureDiagnostic{
				Stage: projectFailureStage(stage), Fault: detail.Fault,
			},
		})
	}
	sealed, valid := output.SealFailures(cause, evidence)
	if !valid {
		return output.ErrContract
	}
	return sealed
}
