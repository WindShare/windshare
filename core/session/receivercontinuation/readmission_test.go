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
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
	"github.com/windshare/windshare/osfs"
)

type handleBoundSource struct{ liveshare.FileSource }

func (handleBoundSource) RevisionContinuity(catalog.NodeRecord) (content.RevisionContinuity, error) {
	return content.OpenHandleRevisionContinuity, nil
}

func TestTransferReadmitsDurableRangesAfterSourceReopen(t *testing.T) {
	t.Run("intact", func(t *testing.T) { testTransferReadmission(t, nil) })
	t.Run("stage removed", func(t *testing.T) { testTransferReadmission(t, os.Remove) })
	t.Run("stage replaced", func(t *testing.T) { testTransferReadmission(t, replaceReadmissionStage) })
}

func testTransferReadmission(t *testing.T, changeStage func(string) error) {
	t.Helper()
	f := newFixtureWithSource(t, false, func(source liveshare.FileSource) liveshare.FileSource {
		return handleBoundSource{source}
	})
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
	output, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: root.RootPath, CreateRoot: root.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, err := transfer.NewSelectionSpec(runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := output.ReserveDirectTree(ctx, selection, receivecontract.NewCatalogRootDirectoryTree())
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok {
		t.Fatal("missing destination reservation")
	}

	var firstRevision, offeredRevision content.FileRevision
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
				offeredRevision = reopened.Current().FileRevision()
				staleSink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error {
					t.Error("a stale reader wrote bytes after the revision changed")
					return transfer.ErrBlockIdentity
				})
				if again := continuation.ReadRange(ctx, lease, descriptor, requested, staleSink); again != reopened {
					t.Errorf("stale reader lost its reopen result: %v", again)
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
		Blocks: reader, Materializer: output, Tracer: tracer, RevisionWait: &revisionwait.Config{GenerationFence: continuation},
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
	if readmission.Previous != firstRevision || readmission.Current == firstRevision || readmission.Current != offeredRevision ||
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
