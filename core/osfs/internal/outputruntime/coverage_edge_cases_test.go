package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RecoveryDecisionEdgeBranches(t *testing.T) {
	var session *Session
	zeroState := fileRecoveryState{}
	zeroIteration := fileRecoveryIteration{}

	if _, err := session.applyInvalidatedRevisionDecision(
		transfer.OutputFile{}, nil, "", resumestate.BoundFileRecord{}, resumestate.RecoveryDecision{}, nil,
	); err == nil {
		t.Fatal("invalidated-revision default action unexpectedly succeeded")
	}
	if _, err := session.applyFileRetirementDecision(
		nil, "", resumestate.BoundFileRecord{}, transfer.OutputFileBinding{}, resumestate.RecoveryDecision{}, nil,
	); err == nil {
		t.Fatal("retirement default action unexpectedly succeeded")
	}
	if _, err := session.installRetirementQuarantine(
		nil, "", resumestate.BoundFileRecord{}, transfer.OutputFileBinding{}, resumestate.RecoveryDecision{}, nil,
	); err == nil {
		t.Fatal("invalid retirement quarantine unexpectedly succeeded")
	}
	if _, err := session.holdInvalidatedRevisionQuarantine(
		transfer.OutputFile{}, resumestate.BoundFileRecord{}, errors.New("observation close failed"),
	); err == nil {
		t.Fatal("invalidated quarantine cleanup failure unexpectedly succeeded")
	}

	if _, err := (&FileTransaction{}).pauseForBeginFileCleanup(
		context.Background(), transfer.FilePauseOutputFailure,
	); err == nil {
		t.Fatal("closed transaction begin-file cleanup unexpectedly succeeded")
	}

	for name, attempt := range map[string]recoveryPublicationAttempt{
		"fixed source changed": {linkErr: outputcap.ErrFixedLinkSourceChanged},
		"ambiguous final link": {linkErr: outputcap.ErrUnsafeNamespace},
		"ordinary final link":  {linkErr: errors.New("link failed")},
		"cleanup only":         {cleanupErr: errors.New("witness close failed")},
	} {
		t.Run(name, func(t *testing.T) {
			_, handled, err := session.handleUnclassifiedRecoveredPublication(
				transfer.OutputFile{}, nil, "", zeroState, attempt,
			)
			if !handled || err == nil {
				t.Fatalf("unclassified publication = handled:%t err:%v", handled, err)
			}
		})
	}

	if _, err := session.applyFileRecoveryAction(
		transfer.OutputFile{}, nil, "", zeroState, zeroIteration,
	); err == nil {
		t.Fatal("zero recovery action unexpectedly succeeded")
	}
}
