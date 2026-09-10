package transfer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

type interruptedReplayCatalog struct {
	CatalogReader
	ready         chan struct{}
	interruptOpen bool
	opens         int
}

func (source *interruptedReplayCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	source.opens++
	// The terminal read and file replay must finish before the worker is allowed
	// to fail. This fixes the interruption at the directory replay boundary.
	const directoryReplayOpen = 3
	if source.opens != directoryReplayOpen {
		return source.CatalogReader.OpenDirectoryPages(ctx, directory)
	}
	if source.interruptOpen {
		close(source.ready)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cursor, err := source.CatalogReader.OpenDirectoryPages(ctx, directory)
	if err != nil {
		return nil, err
	}
	return &interruptedReplayCursor{DirectoryPageCursor: cursor, ready: source.ready}, nil
}

type interruptedReplayCursor struct {
	catalog.DirectoryPageCursor
	ready chan struct{}
}

func (cursor *interruptedReplayCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	close(cursor.ready)
	<-ctx.Done()
	return catalog.CatalogPage{}, false, ctx.Err()
}

type replayGatedRangeReader struct {
	RangeReader
	ready <-chan struct{}
}

func (reader replayGatedRangeReader) ReadRange(
	ctx context.Context,
	handle RevisionHandle,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	select {
	case <-reader.ready:
		return reader.RangeReader.ReadRange(ctx, handle, descriptor, requested, sink)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestTransferJobPreservesWorkerStopDuringCatalogReplay(t *testing.T) {
	for _, boundary := range []string{"open", "next"} {
		t.Run(boundary, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](0x91)
			output := newJobOutput(share)
			output.transactionScript.commitErr = outputFailure(
				fault.ScopeFileLocal, fault.OutputStateIO, errors.New("publication authority was lost"),
			)
			ready := make(chan struct{})
			job, _ := branchJob(t, output, &jobRevisionClient{}, replayGatedRangeReader{
				RangeReader: scriptedRangeReader{}, ready: ready,
			})
			job.catalog = &interruptedReplayCatalog{
				CatalogReader: job.catalog, ready: ready, interruptOpen: boundary == "open",
			}
			var discoveryTrace TransferLifecycleTrace
			job.tracer = TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
				if event.Stage == TransferDiscoveryCompleted {
					discoveryTrace = event
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()

			result := job.Run(ctx)
			expected := mustOutputFault(fault.ScopeFileLocal, fault.OutputStateIO)
			if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != expected ||
				result.SettlementFault != expected || result.TerminationInterruption.Valid() {
				t.Fatalf("worker stop outcome=%v termination=%v settlement=%v interruption=%v",
					result.Outcome, result.TerminationFault, result.SettlementFault, result.TerminationInterruption)
			}
			if result.Progress.Discovery != DiscoveryComplete || discoveryTrace.Stage != TransferDiscoveryCompleted ||
				discoveryTrace.Discovery != DiscoveryComplete || discoveryTrace.Failed || discoveryTrace.Fault.Valid() {
				t.Fatalf("authenticated discovery status=%v trace stage=%v discovery=%v failed=%v fault=%v",
					result.Progress.Discovery, discoveryTrace.Stage, discoveryTrace.Discovery,
					discoveryTrace.Failed, discoveryTrace.Fault)
			}
			transaction := output.transactions["file.bin"]
			if transaction == nil || transaction.commitCalls != 1 || len(transaction.pauseReasons) != 0 ||
				len(transaction.retireReasons) != 0 || output.pauseCalls != 1 || output.completeCalls != 0 {
				t.Fatalf("worker settlement was repeated: transaction=%+v pause=%d complete=%d",
					transaction, output.pauseCalls, output.completeCalls)
			}
		})
	}
}

func TestDiscoveryIsolationPreservesOnlyThePropagatedWorkerCause(t *testing.T) {
	for _, phase := range []string{"initial", "replay"} {
		for _, propagated := range []bool{true, false} {
			name := "independent fault"
			if propagated {
				name = "worker cause"
			}
			t.Run(phase+"/"+name, func(t *testing.T) {
				root := transferID[catalog.DirectoryID](0x92)
				run := &jobRun{job: &TransferJob{root: root}}
				workerCause := newFaultFailure(
					mustOutputFault(fault.ScopeFileLocal, fault.OutputStateIO), errors.New("worker stopped"),
				)
				ctx, cancel := context.WithCancelCause(t.Context())
				cancel(workerCause)
				failure := normalizeCatalogBoundary(ctx, context.Canceled)
				if !propagated {
					// Matching fault values do not prove that an independent catalog
					// failure was caused by the worker's cancellation.
					failure = newFaultFailure(workerCause.policy.value, errors.New("independent catalog failure"))
				}
				var got error
				if phase == "initial" {
					var checkpoint nodeLedgerCheckpoint
					got = run.isolateIncrementalFailure(ctx, checkpoint, root, "", failure)
				} else {
					discovery := incrementalDirectoryDiscovery{
						run: run, request: incrementalDirectoryRequest{directory: root},
					}
					got = discovery.handleReplayFailure(ctx, failure)
				}
				if propagated {
					if got != workerCause {
						t.Fatalf("worker cause was reclassified: %v", got)
					}
				} else if expected := mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration); closedFault(got) != expected {
					t.Fatalf("independent root failure was suppressed: got=%v want=%v", closedFault(got), expected)
				}
			})
		}
	}
}
