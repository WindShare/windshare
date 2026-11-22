package transfer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
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
				stream := newTestDiagnosticsStream(t)
				job.diagnostics = stream.Sink()

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
				var cleanupRec diagnostics.Record
				for _, rec := range drainTestDiagnostics(stream) {
					if rec.Payload().Get("stage").Text() == TransferRevisionCleanupSettled.DiagnosticName() {
						cleanupRec = rec
					}
				}
				wantJobID := diagnostics.Identity(result.TransferJobID.Bytes()).Text()
				if !cleanupRec.Valid() || cleanupRec.Payload().Get("transfer_job_id").Text() != wantJobID ||
					!cleanupRec.Payload().Get("failed").Bool() ||
					cleanupRec.Payload().Get("file_selection").Text() != FileSelectionInherited.DiagnosticName() ||
					cleanupRec.Payload().Get("interruption").Text() != "2" {
					t.Fatalf("cleanup trace=%+v", cleanupRec)
				}
			})
		})
	}
}
