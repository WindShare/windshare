package receivercontinuation

import (
	"bytes"
	"context"
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
	var settled int
	tracer := transfer.TransferLifecycleTraceFunc(func(event transfer.TransferLifecycleTrace) {
		if event.Stage == transfer.TransferFileSettled {
			settled++
		}
		if event.Stage == transfer.TransferFileRevisionReadmission && event.RevisionReadmission.Decision == "readmitted" {
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
