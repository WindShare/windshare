package receivercontinuation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
	"github.com/windshare/windshare/osfs"
)

func TestTransferReadmitsDurableRangesAfterSourceReopen(t *testing.T) {
	t.Run("intact", func(t *testing.T) { testTransferReadmission(t, nil) })
	t.Run("stage removed", func(t *testing.T) { testTransferReadmission(t, os.Remove) })
	t.Run("stage replaced", func(t *testing.T) { testTransferReadmission(t, replaceReadmissionStage) })
}

func testTransferReadmission(t *testing.T, changeStage func(string) error) {
	t.Helper()
	f := newFixture(t)
	runtime, channel := f.connect()
	ctx := t.Context()
	continuation, err := New(ctx, runtime, func(context.Context, *receiversession.Runtime) (*receiversession.Runtime, error) {
		next, _ := f.connect()
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer continuation.Close()

	root := testoutputroot.New(t)
	output, err := osfs.NewOutputAuthority(osfs.OutputConfig{
		RootPath: root.RootPath, CreateRoot: root.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := output.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, err := transfer.NewSelectionSpec(runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := output.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != osfs.OutputLookupCreateReady {
		t.Fatalf("lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Create(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("missing destination reservation")
	}

	var firstRevision content.FileRevision
	var firstRange content.Range
	var cutAt time.Time
	var reconnectedAfter time.Duration
	var transferred, retriedBytes uint64
	reader := observedContinuationReader{Session: continuation, read: func(
		ctx context.Context, lease transfer.RevisionHandle, descriptor content.FileRevisionDescriptor,
		requested content.Range, sink transfer.RangeSink,
	) error {
		err := continuation.ReadRange(ctx, lease, descriptor, requested, transfer.RangeSinkFunc(func(ctx context.Context, offset uint64, data []byte) error {
			transferred += uint64(len(data))
			if descriptor.FileRevision() != firstRevision && !cutAt.IsZero() && offset < firstRange.End {
				retriedBytes += min(uint64(len(data)), firstRange.End-offset)
			}
			return sink.WriteRange(ctx, offset, data)
		}))
		if err != nil {
			if reopened, ok := err.(*content.RevisionReopenedError); ok {
				staleSink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error {
					t.Error("a stale reader wrote bytes after the revision changed")
					return transfer.ErrBlockIdentity
				})
				if again := continuation.ReadRange(ctx, lease, descriptor, requested, staleSink); again != reopened {
					t.Errorf("stale reader lost its reopen result: %v", again)
				}
				if rebound := continuation.ReadRange(ctx, lease, reopened.Current(), requested, staleSink); !errors.Is(rebound, transfer.ErrBlockIdentity) {
					t.Errorf("old handle accepted rebound descriptor: %v", rebound)
				}
			}
			return err
		}
		if cutAt.IsZero() {
			firstRange, firstRevision = requested, descriptor.FileRevision()
			// Explicit retirement avoids a grace-period sleep and forces the same
			// handle-reopen path used after the sender evicts an idle revision.
			dependencies, err := runtime.TransferDependencies()
			if err != nil {
				return err
			}
			if err := dependencies.ReleaseRevision(ctx, lease); err != nil {
				return err
			}
			cutAt = time.Now()
			_ = channel.Close()
			select {
			case <-runtime.Done():
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		} else if reconnectedAfter == 0 {
			if descriptor.FileRevision() == firstRevision || requested.Offset != firstRange.End {
				t.Errorf("first resumed range=%v under revision %x", requested, descriptor.FileRevision())
			}
			reconnectedAfter = time.Since(cutAt)
		}
		return nil
	}}

	var readmission transfer.RevisionReadmissionTrace
	var retainedBeforeReopen uint64
	var stageErr error
	var settled int
	tracer := transfer.TransferLifecycleTraceFunc(func(event transfer.TransferLifecycleTrace) {
		if event.Stage == transfer.TransferFileSettled {
			settled++
		}
		if event.Stage != transfer.TransferFileRevisionReadmission {
			return
		}
		switch event.RevisionReadmission.Decision {
		case "readmit":
			retainedBeforeReopen = event.RevisionReadmission.Retained
			// The pause has flushed and released the stage, so external changes
			// here exercise the next ownership check without racing a writer.
			if changeStage != nil {
				stageErr = changeReadmissionStage(root.RootPath, changeStage)
			}
		case "readmitted":
			readmission = event.RevisionReadmission
		}
	})
	id, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: id, Session: continuation, Catalog: continuation, Revisions: continuation,
		Blocks: reader, Materializer: operation, Tracer: tracer, RevisionWait: &revisionwait.Config{WatchSession: continuation.WaitSessionChange},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(ctx)
	if changeStage != nil {
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		if retainedBeforeReopen == 0 || retainedBeforeReopen != firstRange.Length() {
			t.Fatalf("expected a durable prefix before reopening, got %d bytes", retainedBeforeReopen)
		}
		if result.Outcome != transfer.DirectTreeOutcomePartial || len(result.Files) != 1 || settled != 1 {
			t.Fatalf("expected an isolated file failure, outcome=%d files=%+v settlements=%d progress=%+v",
				result.Outcome, result.Files, settled, result.Progress)
		}
		_, reason, blocked := result.Files[0].Settlement.ItemBlock()
		progress := result.Progress
		if !blocked || reason != transfer.ItemBlockOwnedObjectUnknown || progress.FileOutcomes.OwnedObjectUnknownFiles != 1 {
			t.Fatalf("expected a file ownership block, got %+v", result)
		}
		if progress.RestartReusableBytes != 0 || progress.VerifiedBytes != 0 || progress.NewlyVerifiedBytes != 0 ||
			!progress.CountersExact || transferred != retainedBeforeReopen {
			t.Fatalf("unavailable stage affected counted progress: %+v, transferred=%d", progress, transferred)
		}
		return
	}
	total := uint64(len(f.payload))
	if result.Outcome != transfer.DirectTreeOutcomeSuccess ||
		result.SucceededFiles != 1 || result.Progress.VerifiedBytes != total ||
		result.Progress.NewlyVerifiedBytes != total || result.Progress.RestartReusableBytes != 0 ||
		!result.Progress.CountersExact || settled != 1 {
		t.Fatalf("result=%+v file settlements=%d", result, settled)
	}
	if readmission.Previous != firstRevision || readmission.Current == firstRevision || readmission.Current.IsZero() ||
		readmission.ComparisonReason != content.ComparisonMetadataAgreement ||
		readmission.Retained != firstRange.Length() || readmission.Reused != firstRange.Length() ||
		transferred != total || retriedBytes != 0 || reconnectedAfter <= 0 {
		t.Fatalf("readmission=%+v transferred=%d retransmitted=%d time to resume=%v", readmission, transferred, retriedBytes, reconnectedAfter)
	}
	destination, _ := intent.MaterializationPlan().DestinationReservation()
	written, err := os.ReadFile(filepath.Join(root.RootPath, destination.PhysicalName(), "source.bin"))
	if err != nil || !bytes.Equal(written, f.payload) {
		t.Fatalf("published %d bytes, error=%v", len(written), err)
	}
	t.Logf("time to resume=%v, checkpoint readmission=%v, reused=%d bytes, retransmitted=%d bytes",
		reconnectedAfter, readmission.Elapsed, readmission.Reused, retriedBytes)
}

func changeReadmissionStage(root string, change func(string) error) error {
	var stage string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".stage" {
			if stage != "" {
				return errors.New("readmission fixture has multiple staged files")
			}
			stage = path
		}
		return nil
	})
	if err != nil {
		return err
	}
	if stage == "" {
		return errors.New("readmission fixture has no staged file")
	}
	return change(stage)
}

func replaceReadmissionStage(path string) error {
	// Keep the physical size so the replacement must fail the ownership check.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.Rename(path, path+".original"); err != nil {
		return err
	}
	return os.WriteFile(path, make([]byte, info.Size()), 0o600)
}

func TestRecoverRefreshFailurePreservesAuthenticatedSourceOutcomesOnPathRetirement(t *testing.T) {
	f := newFixture(t)
	retired, ch := f.connect()
	ctx := t.Context()
	replacements := 0
	continuation, err := New(ctx, retired, func(context.Context, *receiversession.Runtime) (*receiversession.Runtime, error) {
		replacements++
		next, _ := f.connect()
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer continuation.Close()

	// Retire the runtime so AwaitPathRetirement(ctx) deterministically returns true.
	_ = ch.Close()
	select {
	case <-retired.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !retired.PathsExhausted() {
		t.Fatal("expected retired runtime paths to be exhausted")
	}

	geometry1, _ := content.NewFileGeometry(64, catalog.MinChunkSize)
	geometry2, _ := content.NewFileGeometry(128, catalog.MinChunkSize)
	meta, _ := content.SourceMetadataFromBytes(bytes.Repeat([]byte{0x55}, 32))
	share := retired.Descriptor().ShareInstance()
	fileID := catalog.FileID{0x11}
	prevBase, _ := content.NewFileRevisionDescriptor(share, fileID, content.FileRevision{1}, geometry1, catalog.ModifiedTime{})
	prev := content.BindRevisionMetadata(prevBase, meta)
	sameMetaBase, _ := content.NewFileRevisionDescriptor(share, fileID, content.FileRevision{2}, geometry1, catalog.ModifiedTime{})
	sameMeta := content.BindRevisionMetadata(sameMetaBase, meta)
	changedSize, _ := content.NewFileRevisionDescriptor(share, fileID, content.FileRevision{3}, geometry2, catalog.ModifiedTime{})

	changedFault, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceRevisionChanged)
	invalidatedFault, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceRevisionInvalidated)
	permanentFault, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourcePermanent)

	cases := []struct {
		name string
		err  error
	}{
		{"reopened mismatch", content.NewRevisionReopenedError(prev, changedSize)},
		{"reopened reusable", content.NewRevisionReopenedError(prev, sameMeta)},
		{"source revision changed", fault.Wrap(changedFault, content.ErrRevisionStale)},
		{"source revision invalidated", fault.Wrap(invalidatedFault, content.ErrRevisionDrift)},
		{"source permanent", fault.Wrap(permanentFault, content.ErrRevisionNotFound)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			retry, got := continuation.recoverRefreshFailure(ctx, retired, tc.err)
			if retry || got != tc.err {
				t.Fatalf("recoverRefreshFailure=(%v, %v), want (false, %v)", retry, got, tc.err)
			}
			if replacements != 0 {
				t.Fatalf("unexpected generation replacements=%d", replacements)
			}
		})
	}

	// Transport/runtime close errors must still trigger generation recovery.
	retry, got := continuation.recoverRefreshFailure(ctx, retired, session.ErrRuntimeClosed)
	if !retry || got != nil || replacements != 1 {
		t.Fatalf("transport recovery=(%v, %v) replacements=%d, want (true, nil) with 1 replacement", retry, got, replacements)
	}
}
