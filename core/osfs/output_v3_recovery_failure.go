package osfs

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/transfer"
)

var errOutputV3PositiveEntryEvidence = errors.New("osfs: output operation failed after positive entry evidence")

type outputV3RecoveryBoundary uint8

const (
	outputV3BeforeEntryEvidence outputV3RecoveryBoundary = iota + 1
	outputV3ExistingEntryUnclassified
	outputV3AuthorizedMutation
)

type outputV3RecoveryFailureDisposition uint8

const (
	outputV3RecoveryPauseRequired outputV3RecoveryFailureDisposition = iota + 1
	outputV3RecoveryAmbiguous
)

// classifyOutputV3RecoveryFailure separates lack of operational authority from
// ambiguous namespace evidence. The former preserves the deterministic cut for
// retry; the latter must never turn a pathname into cleanup authority.
func classifyOutputV3RecoveryFailure(
	cause error,
	boundary outputV3RecoveryBoundary,
) outputV3RecoveryFailureDisposition {
	if cause == nil {
		return 0
	}
	if boundary == outputV3ExistingEntryUnclassified || errors.Is(cause, errOutputV3PositiveEntryEvidence) ||
		errors.Is(cause, errOutputV3Unsafe) || errors.Is(cause, errOutputV3Collision) {
		return outputV3RecoveryAmbiguous
	}
	return outputV3RecoveryPauseRequired
}

func recoveryFileOutputFault(
	operation string,
	cause error,
	boundary outputV3RecoveryBoundary,
) error {
	fault := fileOutputFault(operation, cause)
	if classifyOutputV3RecoveryFailure(cause, boundary) == outputV3RecoveryPauseRequired {
		return pauseRequiredFileOutputFault(fault)
	}
	return fault
}

func pauseRequiredFileOutputFault(cause error) error {
	return transfer.NewOutputSessionError(fileSettlementFailure(cause), true)
}

func pauseRequiredFileOperationFault(
	operation string,
	operationErr error,
	cleanupErr error,
) error {
	var result error
	if errors.Is(operationErr, errOutputAncestryUnsafe) {
		result = outputAncestryPauseFault(operation, operationErr)
	} else if operationErr != nil {
		result = pauseRequiredFileOutputFault(fileOutputFault(operation, operationErr))
	}
	if cleanupErr != nil {
		result = errors.Join(result, pauseRequiredFileOutputFault(outputFault(
			transfer.OutputFaultFile,
			transfer.OutputFaultStateIO,
			fmt.Errorf("clean up after %s: %w", operation, cleanupErr),
		)))
	}
	return result
}
