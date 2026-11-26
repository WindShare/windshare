package transfer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
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

	t.Run("mid-release parent deadline preserves DeadlineExceeded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			output := newJobOutput(transferID[catalog.ShareInstance](95))
			revisions := &jobRevisionClient{}
			job, file := branchJob(t, output, revisions, scriptedRangeReader{})
			job.settlementTimeout = DefaultOutputSettlementTimeout
			stream := newTestDiagnosticsStream(t)
			job.diagnostics = stream.Sink()

			type ctxKey struct{}
			customDeadlineCause := errors.New("caller timeout cause")
			parent, cancel := context.WithTimeoutCause(
				context.WithValue(context.Background(), ctxKey{}, "trace-value"),
				200*time.Millisecond,
				customDeadlineCause,
			)
			defer cancel()

			var observedValue any
			var observedErr, observedCause error
			job.revisions = blockingReleaseRevisionClient{
				jobRevisionClient: revisions,
				onRelease: func(releaseCtx context.Context, _ RevisionHandle) error {
					observedValue = releaseCtx.Value(ctxKey{})
					<-releaseCtx.Done()
					observedErr = releaseCtx.Err()
					observedCause = context.Cause(releaseCtx)
					return releaseCtx.Err()
				},
			}

			result := job.Run(parent)
			if observedValue != "trace-value" {
				t.Fatalf("release ctx value = %v, want trace-value", observedValue)
			}
			if observedErr != context.DeadlineExceeded || observedCause != context.DeadlineExceeded {
				t.Fatalf("release ctx err=%v cause=%v, want context.DeadlineExceeded", observedErr, observedCause)
			}
			if len(revisions.released) != 1 || len(result.RevisionCleanupFailures) != 1 {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
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
			if !cleanupRec.Valid() || cleanupRec.Payload().Get("interruption").Text() != "2" {
				t.Fatalf("cleanup trace=%+v", cleanupRec)
			}
		})
	})

	t.Run("sibling BoundaryError cancel cause is stripped from release context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			output := newJobOutput(transferID[catalog.ShareInstance](96))
			revisions := &jobRevisionClient{}
			job, _ := branchJob(t, output, revisions, scriptedRangeReader{})
			job.settlementTimeout = DefaultOutputSettlementTimeout

			siblingFault := sourceUnavailableFailure(errors.New("sibling failed"))
			parent, cancel := context.WithCancelCause(context.Background())
			cancel(siblingFault)

			releaseCtx, stop := job.newRevisionReleaseContext(parent)
			defer stop()
			if releaseCtx.Err() != context.Canceled || context.Cause(releaseCtx) != context.Canceled {
				t.Fatalf("release ctx err=%v cause=%v, want standard context.Canceled", releaseCtx.Err(), context.Cause(releaseCtx))
			}
			if fault.ContextCause(releaseCtx) != nil {
				t.Fatalf("leaked sibling boundary error: %v", fault.ContextCause(releaseCtx))
			}
		})
	})
}

type gatedDirectoryCatalog struct {
	CatalogReader
	gateDir catalog.DirectoryID
	ready   <-chan struct{}
}

func (c gatedDirectoryCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if directory == c.gateDir {
		select {
		case <-c.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.CatalogReader.OpenDirectoryPages(ctx, directory)
}

type blockingUntilCanceledRangeReader struct {
	once    sync.Once
	entered chan struct{}
}

func (r *blockingUntilCanceledRangeReader) ReadRange(
	ctx context.Context,
	_ RevisionHandle,
	_ content.FileRevisionDescriptor,
	_ content.Range,
	_ RangeSink,
) error {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func TestDiscoveryFailureReleasesActiveRevisionWithLiveCallerContext(t *testing.T) {
	newDiscoveryFailureJob := func(
		t *testing.T,
		seed byte,
		childFailure error,
		admitFailure error,
		onRelease func(context.Context, RevisionHandle) error,
	) (*TransferJob, *jobRevisionClient, catalog.FileID, RevisionHandle) {
		t.Helper()
		share := transferID[catalog.ShareInstance](seed)
		root := transferID[catalog.DirectoryID](seed + 1)
		child := transferID[catalog.DirectoryID](seed + 2)
		file := transferID[catalog.FileID](seed + 3)
		handle := transferID[RevisionHandle](seed + 4)
		descriptor := jobDescriptor(t, share, file, 1, uint64(catalog.MinChunkSize))
		opened, err := NewOpenedRevision(handle, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := NewSelectionRules(true, nil)
		if err != nil {
			t.Fatal(err)
		}
		snapshots := map[catalog.DirectoryID]catalog.DirectorySnapshot{
			root: jobSnapshot(
				t, share, root, 1,
				jobEntry(t, file, "active.bin", uint64(catalog.MinChunkSize)),
				jobDirectoryEntry(t, child, "child"),
			),
		}
		failures := make(map[catalog.DirectoryID]error)
		if childFailure != nil {
			failures[child] = childFailure
		} else {
			snapshots[child] = jobSnapshot(t, share, child, 2)
		}
		output := newJobOutput(share)
		if admitFailure != nil {
			output.ensureFailures = map[string]error{"windshare/child": admitFailure}
		}
		revisions := &jobRevisionClient{
			opened:   map[catalog.FileID]OpenedRevision{file: opened},
			failures: make(map[catalog.FileID]error),
		}
		workerEntered := make(chan struct{})
		job, err := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: gatedDirectoryCatalog{
				CatalogReader: failingCatalog{snapshots: snapshots, failures: failures},
				gateDir:       child,
				ready:         workerEntered,
			},
			Revisions: blockingReleaseRevisionClient{
				jobRevisionClient: revisions,
				onRelease:         onRelease,
			},
			Blocks:            &blockingUntilCanceledRangeReader{entered: workerEntered},
			Materializer:      output,
			SettlementTimeout: DefaultOutputSettlementTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		return job, revisions, file, handle
	}

	t.Run("catalog integrity failure against live session sends release without caller interruption", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var releaseCtxErr error
			var releaseHasDeadline bool
			job, revisions, file, handle := newDiscoveryFailureJob(
				t, 0xa0,
				catalogIntegrityFailure(ErrCatalogIdentity),
				nil,
				func(releaseCtx context.Context, _ RevisionHandle) error {
					releaseCtxErr = releaseCtx.Err()
					_, releaseHasDeadline = releaseCtx.Deadline()
					return nil
				},
			)
			stream := newTestDiagnosticsStream(t)
			job.diagnostics = stream.Sink()

			result := job.Run(context.Background())
			if releaseCtxErr != nil || !releaseHasDeadline {
				t.Fatalf("release ctx err=%v hasDeadline=%t, want live bounded context", releaseCtxErr, releaseHasDeadline)
			}
			wantFault := mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration)
			if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != wantFault ||
				result.TerminationInterruption != 0 || len(revisions.released) != 1 || revisions.released[0] != handle ||
				len(result.RevisionCleanupFailures) != 0 {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
			}
			var cleanupRec diagnostics.Record
			for _, rec := range drainTestDiagnostics(stream) {
				if rec.Payload().Get("stage").Text() == TransferRevisionCleanupSettled.DiagnosticName() {
					cleanupRec = rec
				}
			}
			if !cleanupRec.Valid() || cleanupRec.Payload().Get("failed").Bool() ||
				cleanupRec.Payload().Get("file_id").Text() != diagnostics.Identity(file.Bytes()).Text() {
				t.Fatalf("cleanup trace=%+v", cleanupRec)
			}
		})
	})

	t.Run("output admission failure keeps release live until outer caller cancels", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			releaseEntered := make(chan struct{})
			var initialReleaseErr error
			admitErr := outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("disk full"))
			job, revisions, file, handle := newDiscoveryFailureJob(
				t, 0xb0,
				nil,
				admitErr,
				func(releaseCtx context.Context, _ RevisionHandle) error {
					initialReleaseErr = releaseCtx.Err()
					close(releaseEntered)
					<-releaseCtx.Done()
					return releaseCtx.Err()
				},
			)

			done := make(chan JobResult, 1)
			go func() { done <- job.Run(ctx) }()
			<-releaseEntered
			started := time.Now()
			cancel()
			result := <-done
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Fatalf("caller cancel mid-release stalled for %v", elapsed)
			}
			if initialReleaseErr != nil {
				t.Fatalf("release ctx started canceled before caller cancel: %v", initialReleaseErr)
			}
			if result.Outcome != DirectTreeOutcomePaused ||
				result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputStateIO) ||
				len(revisions.released) != 1 || revisions.released[0] != handle ||
				len(result.RevisionCleanupFailures) != 1 ||
				result.RevisionCleanupFailures[0].FileID != file ||
				result.RevisionCleanupFailures[0].Interruption != TransferInterruptionCanceled {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
			}
		})
	})

	t.Run("dead session reports transport cleanup fault instead of fabricated caller cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var releaseCtxErr error
			transportErr := sessionTransportFailure(errors.New("session closed"))
			job, revisions, file, handle := newDiscoveryFailureJob(
				t, 0xc0,
				transportErr,
				nil,
				func(releaseCtx context.Context, _ RevisionHandle) error {
					releaseCtxErr = releaseCtx.Err()
					return sessionTransportFailure(errors.New("runtime closed"))
				},
			)

			result := job.Run(context.Background())
			if releaseCtxErr != nil {
				t.Fatalf("release ctx err=%v, want nil (session failure should come from client)", releaseCtxErr)
			}
			wantFault := mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport)
			if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != wantFault ||
				len(revisions.released) != 1 || revisions.released[0] != handle ||
				len(result.RevisionCleanupFailures) != 1 ||
				result.RevisionCleanupFailures[0].FileID != file ||
				result.RevisionCleanupFailures[0].Fault != wantFault ||
				result.RevisionCleanupFailures[0].Interruption != 0 {
				t.Fatalf("result=%+v released=%v", result, revisions.released)
			}
		})
	})
}
