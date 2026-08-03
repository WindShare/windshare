//go:build linux

package linuxsubreaper

import (
	"errors"
	"os"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

func failedSettlement(request ownerprotocol.Request, errorCode string, cause error) ownerprotocol.Settlement {
	message := "linux process owner could not initialize"
	if cause != nil {
		message = boundedDiagnostic(cause)
	}
	active := uint32(0)
	return ownerprotocol.Settlement{
		SchemaVersion:     ownerprotocol.SettlementSchemaVersion,
		Identity:          request.Identity,
		TerminationReason: ownerprotocol.TerminationInitializationFailed,
		Target: ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetNotStarted, FailureCode: errorCode, FailureMessage: message,
		},
		Input:     ownerprotocol.InputEvidence{Outcome: unstartedInputOutcome(request.Command.Stdin)},
		TreeState: ownerprotocol.TreeProvenEmpty,
		Cleanup:   ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted},
		Platform: ownerprotocol.PlatformEvidence{
			Kind: ownerprotocol.PlatformLinuxSubreaper, OwnerPID: os.Getpid(), ActiveProcessCount: &active,
		},
	}
}

func unstartedInputOutcome(input *ownerprotocol.Stdin) string {
	if input == nil {
		return ownerprotocol.InputNotRequested
	}
	return ownerprotocol.InputNotStarted
}

func classifyInputEvidence(authority *ownerprotocol.Stdin, deliveryErr error) ownerprotocol.InputEvidence {
	switch {
	case deliveryErr != nil:
		return ownerprotocol.InputEvidence{
			Outcome: ownerprotocol.InputFailed, FailureCode: "CHILD_STDIN_DELIVERY_FAILED",
			FailureMessage: boundedDiagnostic(deliveryErr),
		}
	case authority == nil:
		return ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}
	default:
		return ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputDelivered}
	}
}

func settleOwnershipEvidence(
	status *ownerprotocol.Settlement,
	authority *inventoryAuthority,
	state supervisionState,
	treeEmpty bool,
	cleanupErr error,
) {
	status.TerminationReason = state.terminationReason
	status.Platform.InventoryScans = authority.scans
	status.Platform.MaximumObservedDescendants = authority.maximumDescendants
	if treeEmpty {
		active := uint32(0)
		status.TreeState = ownerprotocol.TreeProvenEmpty
		status.Platform.ActiveProcessCount = &active
		status.Platform.QuietInventoryCount = quietInventoryCount
	} else {
		status.TreeState = ownerprotocol.TreeUnknown
		status.Platform.ActiveProcessCount = nil
		if cleanupErr == nil {
			cleanupErr = errors.New("owned process tree did not reach a proven-empty state")
		}
	}
	if cleanupErr == nil {
		status.Cleanup = ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted}
	} else {
		status.Cleanup = ownerprotocol.CleanupEvidence{
			Outcome: ownerprotocol.CleanupFailed, FailureCode: "OWNERSHIP_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(cleanupErr),
		}
	}
	cause := errors.Join(state.authorityFailure, cleanupErr)
	if cause == nil {
		return
	}
	status.OwnerFailure = &ownerprotocol.FailureEvidence{
		Code: "OWNER_AUTHORITY_FAILED", Message: boundedDiagnostic(cause),
	}
	if state.launched() && status.Target.Outcome == ownerprotocol.TargetNotStarted {
		status.Target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetTerminalEvidenceLost, FailureCode: "TERMINAL_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(cause),
		}
		if status.Platform.Root != nil {
			status.Platform.Root.State = ownerprotocol.RootTerminalEvidenceLost
			status.Platform.Root.ExitCode = nil
			status.Platform.Root.Signal = ""
		}
	} else if !state.launched() && status.Target.Outcome == ownerprotocol.TargetNotStarted {
		status.Target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetNotStarted, FailureCode: "OWNERSHIP_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(cause),
		}
	}
}

func validateSettlement(settlement ownerprotocol.Settlement, request ownerprotocol.Request) error {
	if err := ownerprotocol.ValidateSettlementForRequest(settlement, request); err != nil {
		return errors.New("linux process owner produced invalid settlement: " + err.Error())
	}
	return nil
}
