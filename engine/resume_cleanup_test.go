package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type resumeCleanupDiagnosticError struct {
	stage osfs.FilesystemOutputFailureStage
	cause error
}

func (failure resumeCleanupDiagnosticError) Error() string { return failure.cause.Error() }
func (failure resumeCleanupDiagnosticError) Unwrap() error { return failure.cause }
func (failure resumeCleanupDiagnosticError) FilesystemOutputDiagnostic() osfs.FilesystemOutputDiagnostic {
	return osfs.FilesystemOutputDiagnostic{Stage: failure.stage}
}

func TestRecoveryRetainsIndependentCloseFailuresAlongsidePrimaryFailure(t *testing.T) {
	for _, operation := range []string{"inspect", "discard without report", "discard cleanup pending"} {
		t.Run(operation, func(t *testing.T) {
			application, err := New(Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close(context.Background())

			primaryCause := errors.New("checkpoint reconciliation failed")
			firstCloseCause := errors.New("operation lease close failed")
			secondCloseCause := errors.New("destination authority close failed")
			primary := resumeCleanupDiagnosticError{stage: osfs.FilesystemOutputFailureCheckpointReconciliation, cause: primaryCause}
			firstClose := resumeCleanupDiagnosticError{stage: osfs.FilesystemOutputFailureAuthorityClose, cause: firstCloseCause}
			secondClose := resumeCleanupDiagnosticError{stage: osfs.FilesystemOutputFailureAuthorityClose, cause: secondCloseCause}
			mixed := fmt.Errorf("native recovery: %w", errors.Join(primary, errors.Join(firstClose, secondClose)))
			if operation == "inspect" {
				_, err = application.InspectRecovery(context.Background(), resumeTestAuthority{
					list: func(context.Context) (RecoverySnapshot, error) { return RecoverySnapshot{}, mixed },
				})
				var failure *RecoveryFailure
				if !errors.As(err, &failure) || failure.Detail.Stage != osfs.FilesystemOutputFailureCheckpointReconciliation {
					t.Fatalf("primary diagnostic changed: %v", err)
				}
			} else {
				listed := resumeTestOperation(t)
				inventory, inspectErr := application.InspectRecovery(context.Background(), resumeTestAuthority{
					snapshot: RecoverySnapshot{Operations: []RecoveryOperation{listed}},
					discard: func(context.Context, receivecontract.OperationID) (RecoveryDiscardReport, error) {
						if operation == "discard without report" {
							return RecoveryDiscardReport{}, mixed
						}
						return RecoveryDiscardReport{ID: listed.ID, Status: RecoveryDiscardCleanupPending}, mixed
					},
				})
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				result := inventory.Discard(context.Background(), listed.ID)
				err = result.Err
				assertResumeCloseCauses(t, result.CleanupError, primaryCause, firstCloseCause, secondCloseCause)
			}
			for _, cause := range []error{primaryCause, firstCloseCause, secondCloseCause} {
				if !errors.Is(err, cause) {
					t.Fatalf("recovery lost original cause %v: %v", cause, err)
				}
			}
			assertResumeCloseCauses(t, application.Close(context.Background()), primaryCause, firstCloseCause, secondCloseCause)
		})
	}
}

func assertResumeCloseCauses(t *testing.T, actual, primary error, cleanup ...error) {
	t.Helper()
	for _, cause := range cleanup {
		if !errors.Is(actual, cause) {
			t.Fatalf("cleanup lost release cause %v: %v", cause, actual)
		}
	}
	if errors.Is(actual, primary) {
		t.Fatalf("primary failure was misclassified as release failure: %v", actual)
	}
}
