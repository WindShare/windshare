package transfer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
)

type deadlineRevisionClient struct{ *jobRevisionClient }

func (client deadlineRevisionClient) ReleaseRevision(ctx context.Context, handle RevisionHandle) error {
	_ = client.jobRevisionClient.ReleaseRevision(ctx, handle)
	<-ctx.Done()
	return ctx.Err()
}

func TestPublishedOutputSurvivesRemoteCleanupDeadline(t *testing.T) {
	for _, finalizeFails := range []bool{false, true} {
		name := "published"
		if finalizeFails {
			name = "output finalization still authoritative"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				output := newJobOutput(transferID[catalog.ShareInstance](91))
				revisions := &jobRevisionClient{}
				job, file := branchJob(t, output, revisions, scriptedRangeReader{})
				job.revisions = deadlineRevisionClient{revisions}
				job.settlementTimeout = time.Second
				if finalizeFails {
					output.finishErr = outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("journal cannot commit"))
				}
				var cleanupTrace TransferLifecycleTrace
				job.tracer = TransferLifecycleTraceFunc(func(trace TransferLifecycleTrace) {
					if trace.Stage == TransferRevisionCleanupSettled {
						cleanupTrace = trace
					}
				})

				result := job.Run(context.Background())
				wantOutcome := DirectTreeOutcomeSuccess
				if finalizeFails {
					wantOutcome = DirectTreeOutcomePaused
				}
				if result.Outcome != wantOutcome || result.SucceededFiles != 1 || len(result.Files) != 0 ||
					result.TerminationCause != nil || output.completeCalls != 1 || output.pauseCalls != 0 {
					t.Fatalf("publication outcome=%+v output=%+v", result, output)
				}
				if (result.SettlementFailure != nil) != finalizeFails {
					t.Fatalf("output finalization lost authority: %+v", result)
				}
				if len(result.RevisionCleanupFailures) != 1 || len(revisions.released) != 1 {
					t.Fatalf("cleanup ownership lost: result=%+v released=%v", result, revisions.released)
				}
				cleanup := result.RevisionCleanupFailures[0]
				if cleanup.FileID != file || cleanup.Interruption != TransferInterruptionDeadline ||
					!errors.Is(cleanup.Cause, context.DeadlineExceeded) {
					t.Fatalf("cleanup diagnostic=%+v", cleanup)
				}
				if cleanupTrace.TransferJobID != result.TransferJobID || !cleanupTrace.Failed ||
					cleanupTrace.FileSelection != FileSelectionInherited || cleanupTrace.Interruption != TransferInterruptionDeadline {
					t.Fatalf("cleanup trace=%+v", cleanupTrace)
				}
			})
		})
	}
}
