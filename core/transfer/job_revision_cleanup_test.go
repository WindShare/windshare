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

type blockingReleaseRevisionClient struct {
	*jobRevisionClient
	onRelease func(context.Context, RevisionHandle) error
}

func (client blockingReleaseRevisionClient) ReleaseRevision(ctx context.Context, handle RevisionHandle) error {
	_ = client.jobRevisionClient.ReleaseRevision(ctx, handle)
	return client.onRelease(ctx, handle)
}

func TestCanceledTransferRelinquishesRevisionWithoutWaitingForSettlementTimeout(t *testing.T) {
	t.Run("canceled during transfer skips remote wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			output := newJobOutput(transferID[catalog.ShareInstance](92))
			revisions := &jobRevisionClient{}
			job, file := branchJob(t, output, revisions, scriptedRangeReader{})
			job.settlementTimeout = DefaultOutputSettlementTimeout

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			output.transactionScript.afterWrite = cancel
			output.transactionScript.pausePersistsPending = true

			var releaseCtxErr error
			job.revisions = blockingReleaseRevisionClient{
				jobRevisionClient: revisions,
				onRelease: func(releaseCtx context.Context, _ RevisionHandle) error {
					releaseCtxErr = releaseCtx.Err()
					<-releaseCtx.Done()
					return releaseCtx.Err()
				},
			}

			started := time.Now()
			result := job.Run(ctx)
			elapsed := time.Since(started)
			if elapsed >= time.Second {
				t.Fatalf("canceled release stalled for %v", elapsed)
			}
			if !errors.Is(releaseCtxErr, context.Canceled) {
				t.Fatalf("release ctx err = %v, want context.Canceled", releaseCtxErr)
			}
			if result.Outcome != DirectTreeOutcomePaused || result.SettlementFailure != nil ||
				len(revisions.released) != 1 || len(result.RevisionCleanupFailures) != 1 {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
			}
			cleanup := result.RevisionCleanupFailures[0]
			if cleanup.FileID != file || cleanup.Interruption != TransferInterruptionCanceled ||
				!errors.Is(cleanup.Cause, context.Canceled) {
				t.Fatalf("cleanup diagnostic=%+v", cleanup)
			}
		})
	})

	t.Run("nil release on canceled run records no cleanup failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			output := newJobOutput(transferID[catalog.ShareInstance](93))
			revisions := &jobRevisionClient{}
			job, _ := branchJob(t, output, revisions, scriptedRangeReader{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			output.transactionScript.afterWrite = cancel
			output.transactionScript.pausePersistsPending = true

			result := job.Run(ctx)
			if result.Outcome != DirectTreeOutcomePaused || len(revisions.released) != 1 ||
				len(result.RevisionCleanupFailures) != 0 {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
			}
		})
	})

	t.Run("cancel mid-release interrupts wait promptly", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			output := newJobOutput(transferID[catalog.ShareInstance](94))
			revisions := &jobRevisionClient{}
			job, _ := branchJob(t, output, revisions, scriptedRangeReader{})
			job.settlementTimeout = DefaultOutputSettlementTimeout

			ctx, cancel := context.WithCancel(context.Background())
			entered := make(chan struct{})
			job.revisions = blockingReleaseRevisionClient{
				jobRevisionClient: revisions,
				onRelease: func(releaseCtx context.Context, _ RevisionHandle) error {
					close(entered)
					<-releaseCtx.Done()
					return releaseCtx.Err()
				},
			}

			done := make(chan JobResult, 1)
			go func() { done <- job.Run(ctx) }()
			<-entered
			started := time.Now()
			cancel()
			result := <-done
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Fatalf("mid-release cancellation stalled for %v", elapsed)
			}
			if len(revisions.released) != 1 || len(result.RevisionCleanupFailures) != 1 ||
				result.RevisionCleanupFailures[0].Interruption != TransferInterruptionCanceled {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
			}
		})
	})
}
