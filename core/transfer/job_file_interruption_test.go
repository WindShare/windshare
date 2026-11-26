package transfer

import (
	"context"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
)

type lookupDeliveryCancelingOutput struct {
	*jobOutput
	onLookup func() error
}

func (o *lookupDeliveryCancelingOutput) OpenDirectTree(
	ctx context.Context, intent ReceiveIntent,
) (DirectTreeSession, error) {
	session, err := o.jobOutput.OpenDirectTree(ctx, intent)
	if err != nil || session == nil {
		return session, err
	}
	return o, nil
}

func (o *lookupDeliveryCancelingOutput) LookupDelivery(
	context.Context, catalog.FileID, string,
) (HistoricalDelivery, bool, error) {
	return HistoricalDelivery{}, false, o.onLookup()
}

func TestTransferJobRecordsFileInterruptionFact(t *testing.T) {
	diskFull := outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, syscall.ENOSPC)
	diskFullFault := mustOutputFault(fault.ScopeFileLocal, fault.OutputStateIO)

	t.Run("canceled before write", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](0xb1)
		output := newJobOutput(share)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		output.transactionScript.beforeWrite = func() error {
			cancel()
			return context.Canceled
		}
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})

		result := job.Run(ctx)
		if result.Outcome != DirectTreeOutcomePaused ||
			result.TerminationInterruption != TransferInterruptionCanceled ||
			len(result.Files) != 1 ||
			result.Files[0].Settlement.Kind() != FilePaused ||
			result.Files[0].Fault.Valid() ||
			result.Files[0].Interruption != TransferInterruptionCanceled {
			t.Fatalf("canceled before write result=%+v", result)
		}
	})

	t.Run("deadline exceeded before write", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](0xb2)
			output := newJobOutput(share)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			output.transactionScript.beforeWrite = func() error {
				<-ctx.Done()
				return ctx.Err()
			}
			job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})

			result := job.Run(ctx)
			if result.Outcome != DirectTreeOutcomePaused ||
				result.TerminationInterruption != TransferInterruptionDeadline ||
				len(result.Files) != 1 ||
				result.Files[0].Settlement.Kind() != FilePaused ||
				result.Files[0].Fault.Valid() ||
				result.Files[0].Interruption != TransferInterruptionDeadline {
				t.Fatalf("deadline exceeded before write result=%+v", result)
			}
		})
	})

	t.Run("canceled during BeginFile without settlement", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](0xb3)
		output := newJobOutput(share)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		output.beginHook = func(MaterializationFile) { cancel() }
		output.beginErr = context.Canceled
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})

		result := job.Run(ctx)
		if result.Outcome != DirectTreeOutcomePaused ||
			result.TerminationInterruption != TransferInterruptionCanceled ||
			len(result.Files) != 1 ||
			result.Files[0].Settlement.Kind() != 0 ||
			result.Files[0].Fault.Valid() ||
			result.Files[0].Interruption != TransferInterruptionCanceled {
			t.Fatalf("BeginFile cancellation result=%+v", result)
		}
	})

	t.Run("canceled during LookupDelivery without settlement", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](0xb4)
		root := transferID[catalog.DirectoryID](0xb5)
		file := transferID[catalog.FileID](0xb6)
		chunk := uint64(catalog.MinChunkSize)
		opened, err := NewOpenedRevision(transferID[RevisionHandle](0xb7), jobDescriptor(t, share, file, 0xb8, chunk))
		if err != nil {
			t.Fatal(err)
		}
		rules, err := NewSelectionRules(true, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		output := &lookupDeliveryCancelingOutput{
			jobOutput: newJobOutput(share),
			onLookup: func() error {
				cancel()
				return context.Canceled
			},
		}
		job, err := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root: jobSnapshot(t, share, root, 0xb9, jobEntry(t, file, "file.bin", chunk)),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions:    &jobRevisionClient{opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error)},
			Blocks:       scriptedRangeReader{},
			Materializer: output,
		})
		if err != nil {
			t.Fatal(err)
		}

		result := job.Run(ctx)
		if result.Outcome != DirectTreeOutcomePaused ||
			result.TerminationInterruption != TransferInterruptionCanceled ||
			len(result.Files) != 1 ||
			result.Files[0].Settlement.Kind() != 0 ||
			result.Files[0].Fault.Valid() ||
			result.Files[0].Interruption != TransferInterruptionCanceled {
			t.Fatalf("LookupDelivery cancellation result=%+v", result)
		}
	})

	t.Run("disk full output failure leaves interruption zero", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](0xba)
		output := newJobOutput(share)
		output.transactionScript.writeErr = diskFull
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})

		result := job.Run(t.Context())
		if result.Outcome != DirectTreeOutcomePartial ||
			result.TerminationInterruption != 0 ||
			len(result.Files) != 1 ||
			result.Files[0].Fault != diskFullFault ||
			result.Files[0].Interruption != 0 {
			t.Fatalf("disk full result=%+v", result)
		}
	})
}
