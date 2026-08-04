package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSmallContractHelpersFailClosedAtEmptyAuthorities(t *testing.T) {
	if (*PlaintextBudget)(nil).tryReserve(1) {
		t.Fatal("nil plaintext budget admitted bytes")
	}
	rules, err := NewPathSelectionRules([]string{"folder"})
	if err != nil {
		t.Fatal(err)
	}
	if !rules.DirectorySelectedAt(transferID[catalog.DirectoryID](0xb1), "folder", false) {
		t.Fatal("exact selected directory path was not recognized")
	}
	if outputFailureExplicitlyRequiresJobPause(nilChildOutputError{}) {
		t.Fatal("nil child in an output error graph acquired pause authority")
	}
	if err := validateOpenedFile(
		transferID[catalog.ShareInstance](0xb2), catalog.Entry{}, OpenedRevision{},
	); !errors.Is(err, ErrRevisionIdentity) {
		t.Fatalf("non-file revision validation = %v", err)
	}
}

func TestUnadmittedJobFinishPreservesLateCancellation(t *testing.T) {
	cause := errors.New("caller cancelled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	run := &jobRun{job: &TransferJob{tracker: newSelectionTracker()}}
	result := run.finish(ctx)
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, cause) {
		t.Fatalf("cancelled finish = %+v", result)
	}
}

type nilChildOutputError struct{}

func (nilChildOutputError) Error() string   { return "nil-child output error" }
func (nilChildOutputError) Unwrap() []error { return []error{nil} }
