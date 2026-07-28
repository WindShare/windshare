package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3QuarantineReasonMapping(t *testing.T) {
	ambiguous := []resumestate.QuarantineReason{
		resumestate.QuarantinePublicationHistory,
		resumestate.QuarantineFinalMismatch,
		resumestate.QuarantineFinalUnsafe,
		resumestate.QuarantineMetadataMismatch,
	}
	for _, reason := range ambiguous {
		if got := mapQuarantineReason(reason); got != transfer.QuarantinePublicationAmbiguous {
			t.Errorf("quarantine reason %d = %d, want publication ambiguous", reason, got)
		}
	}
	if got := mapQuarantineReason(resumestate.QuarantinePartialObjectCreation); got != transfer.QuarantineRetirementMismatch {
		t.Errorf("partial object creation maps to %d, want retirement mismatch", got)
	}
	for _, reason := range []resumestate.QuarantineReason{
		resumestate.QuarantineUpdateTemporary,
		resumestate.QuarantineOutputObjectDuplicate,
	} {
		if got := mapQuarantineReason(reason); got != transfer.QuarantineStateCorrupt {
			t.Errorf("quarantine reason %d = %d, want state corrupt", reason, got)
		}
	}
	for _, reason := range []resumestate.QuarantineReason{
		0,
		resumestate.QuarantineAnchorMissing,
		resumestate.QuarantineStageUnsafe,
	} {
		if got := mapQuarantineReason(reason); got != transfer.QuarantineOwnershipMismatch {
			t.Errorf("default quarantine reason %d = %d, want ownership mismatch", reason, got)
		}
	}
}

func TestOutputV3RetirementDecisionCleanupAndDefaultEdges(t *testing.T) {
	cleanupErr := errors.New("observation close failed")
	if err := fileRetirementObservationCleanupFault(resumestate.RecoveryDecision{}, nil); err != nil {
		t.Fatalf("nil cleanup error = %v", err)
	}
	if err := fileRetirementObservationCleanupFault(resumestate.RecoveryDecision{}, cleanupErr); err == nil {
		t.Fatal("ordinary retirement action accepted cleanup failure")
	}
	step, err := (*Session)(nil).applyFileRetirementDecision(
		nil, "", resumestate.BoundFileRecord{}, transfer.OutputFileBinding{}, resumestate.RecoveryDecision{}, nil,
	)
	if !step.complete {
		t.Fatal("unexpected retirement action did not complete")
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Code() != transfer.OutputFaultContract {
		t.Fatalf("default retirement action error = %v, want contract output fault", err)
	}
}

func TestOutputV3FileFaultClassificationEdges(t *testing.T) {
	regular := errors.New("disk write failed")
	if err := fileOutputFault("write stage", regular); !errors.Is(err, regular) {
		t.Fatalf("regular file fault lost cause: %v", err)
	}
	if err := fileOutputFault("remove stage", outputcap.ErrUnsafeNamespace); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe file fault lost namespace cause: %v", err)
	}
	if err := fileSettlementFailure(regular); !errors.Is(err, regular) {
		t.Fatalf("settlement failure lost cause: %v", err)
	}
	if existing := outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultStateIO, regular); fileSettlementFailure(existing) != existing {
		t.Fatal("existing output fault was unnecessarily wrapped")
	}
}
