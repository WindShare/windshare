package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// closeOnlyRecoveryFile lets the tests drive cleanup branches without opening a
// real filesystem object. The embedded interface keeps the fixture deliberately
// narrow because these helpers only require Close.
type closeOnlyRecoveryFile struct {
	outputcap.File
	closeErr error
}

func (file closeOnlyRecoveryFile) Close() error { return file.closeErr }

func TestOutputV3RecoveryBranchErrorEdges(t *testing.T) {
	var digest resumestate.LocatorDigest
	var bound resumestate.BoundFileRecord
	var decision resumestate.UpdateTemporaryDecision

	if _, _, err := (&Session{}).installGateFileStateQuarantine(
		transfer.OutputFile{}, nil, resumestate.ShardedName{}, bound, decision, digest,
	); err == nil {
		t.Fatal("invalid update-temporary decision unexpectedly succeeded")
	}
	if _, err := (&Session{}).installInvalidatedRevisionQuarantine(
		transfer.OutputFile{}, nil, "", bound, resumestate.RecoveryDecision{}, nil,
	); err == nil {
		t.Fatal("invalid recovery decision unexpectedly succeeded")
	}
	if _, err := (&Session{}).installFileRecoveryDecision(
		transfer.OutputFile{}, nil, "", fileRecoveryState{}, fileRecoveryIteration{},
	); err == nil {
		t.Fatal("invalid file recovery decision unexpectedly succeeded")
	}
	if _, err := (&Session{}).finishInstalledRetirement(
		transfer.OutputFile{}, nil, "", fileRecoveryState{}, resumestate.RecoveryDecision{},
	); err == nil {
		t.Fatal("invalid retirement binding unexpectedly succeeded")
	}
	if _, err := (&Session{}).settleClassifiedRecoveredPublication(
		transfer.OutputFile{}, nil, "", fileRecoveryState{}, recoveryPublicationAttempt{},
	); err == nil {
		t.Fatal("invalid classified publication unexpectedly succeeded")
	}
	if result, err := (&Session{}).finishInstalledFileRecovery(
		transfer.OutputFile{}, nil, "", fileRecoveryState{}, fileRecoveryIteration{},
	); err != nil || result.terminal {
		t.Fatalf("default installed recovery action = %+v, %v; want continuation", result, err)
	}

	// A zero session reaches the settlement constructor after the hold decision;
	// the constructor must reject the missing durable reference rather than panic.
	if _, err := (&Session{}).holdInvalidatedRevisionQuarantine(
		transfer.OutputFile{}, bound, nil,
	); err == nil {
		t.Fatal("hold quarantine accepted a zero session reference")
	}

	authorizationErr := errors.New("unauthorized")
	if err := closeGateUnauthorizedTemporary(closeOnlyRecoveryFile{}, authorizationErr); !errors.Is(err, authorizationErr) {
		t.Fatalf("authorization error lost when close succeeds: %v", err)
	}
	closeErr := errors.New("close failed")
	if err := closeGateUnauthorizedTemporary(closeOnlyRecoveryFile{closeErr: closeErr}, authorizationErr); !errors.Is(err, closeErr) {
		t.Fatalf("close error lost when authorization fails: %v", err)
	}
}
