package filesystem

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/osfs"
)

func projectFailureStage(stage osfs.FilesystemOutputFailureStage) output.FailureStage {
	switch stage {
	case osfs.FilesystemOutputFailureDestinationBinding:
		return output.FailureDestinationBinding
	case osfs.FilesystemOutputFailureInventoryPaging:
		return output.FailureInventory
	case osfs.FilesystemOutputFailureActiveLookup:
		return output.FailureLookup
	case osfs.FilesystemOutputFailureOperationAcquisition:
		return output.FailureOpen
	case osfs.FilesystemOutputFailureOperationAdmission:
		return output.FailureCreate
	case osfs.FilesystemOutputFailureCheckpointReconciliation:
		return output.FailureCheckpoint
	case osfs.FilesystemOutputFailureNativeDurability:
		return output.FailureDurability
	case osfs.FilesystemOutputFailureAuthorityClose:
		return output.FailureClose
	default:
		return 0
	}
}

func sealDiagnostic(cause error, diagnostic osfs.FilesystemOutputDiagnostic) error {
	failure, _ := observation.ProjectNormalizedFault(diagnostic.FaultDomain, diagnostic.NormalizedScope, diagnostic.NormalizedCode)
	sealed, ok := output.SealFailure(cause, output.FailureDiagnostic{
		Stage: projectFailureStage(diagnostic.Stage), Failure: failure,
	})
	if !ok {
		return output.ErrContract
	}
	return sealed
}

// SealFilesystemOutputFailure accepts value-only native evidence at the adapter
// boundary; engine consumers only see the normalized application failure.
func SealFilesystemOutputFailure(diagnostic osfs.FilesystemOutputDiagnostic) (error, bool) {
	if !diagnostic.Valid() {
		return nil, false
	}
	cause := fmt.Errorf("filesystem output failed at %s", diagnostic.Stage)
	return sealDiagnostic(cause, diagnostic), true
}

func sealFilesystemOutputFailure(cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, osfs.ErrResumeStateBusy) {
		cause = errors.Join(output.ErrBusy, cause)
	}
	diagnostic, ok := osfs.FilesystemOutputDiagnosticFor(cause)
	if !ok {
		return cause
	}

	stages := []osfs.FilesystemOutputFailureStage{diagnostic.Stage}
	for stage := osfs.FilesystemOutputFailureDestinationBinding; stage <= osfs.FilesystemOutputFailureAuthorityClose; stage++ {
		if stage != diagnostic.Stage {
			stages = append(stages, stage)
		}
	}
	var evidence []output.FailureEvidence
	for _, stage := range stages {
		matching := osfs.FilesystemOutputFailureForStage(cause, stage)
		if matching == nil {
			continue
		}
		detail, found := osfs.FilesystemOutputDiagnosticFor(matching)
		if !found {
			continue
		}
		failure, _ := observation.ProjectNormalizedFault(detail.FaultDomain, detail.NormalizedScope, detail.NormalizedCode)
		evidence = append(evidence, output.FailureEvidence{Cause: matching, Diagnostic: output.FailureDiagnostic{
			Stage: projectFailureStage(stage), Failure: failure,
		}})
	}
	sealed, valid := output.SealFailures(cause, evidence)
	if !valid {
		return output.ErrContract
	}
	return sealed
}

// FailureDiagnostic retains native evidence for host diagnostics without making
// the shared recovery model depend on filesystem-specific classifications.
func FailureDiagnostic(cause error) (osfs.FilesystemOutputDiagnostic, bool) {
	return osfs.FilesystemOutputDiagnosticFor(cause)
}
