package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type pathScriptedJobOutput struct {
	*jobOutput
	scripts map[string]jobTransactionScript
}

func (output *pathScriptedJobOutput) OpenSelection(
	ctx context.Context,
	selection OutputSelection,
) (OutputSession, error) {
	if _, err := output.jobOutput.OpenSelection(ctx, selection); err != nil {
		return nil, err
	}
	return output, nil
}

func (output *pathScriptedJobOutput) BeginFile(
	ctx context.Context,
	file OutputFile,
) (FileStart, error) {
	start, err := output.jobOutput.BeginFile(ctx, file)
	if err != nil {
		return FileStart{}, err
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		return start, nil
	}
	if script, scripted := output.scripts[file.Path]; scripted {
		transaction.(*jobFileTransaction).script = script
	}
	return start, nil
}

func TestTransferJobIsolatesTransactionQuarantineAndPublishesSibling(t *testing.T) {
	share := transferID[catalog.ShareInstance](180)
	root := transferID[catalog.DirectoryID](181)
	quarantinedFile := transferID[catalog.FileID](182)
	publishedFile := transferID[catalog.FileID](183)
	base := newJobOutput(share)
	base.completeSettlement = JobPausedNeedsAttention
	output := &pathScriptedJobOutput{
		jobOutput: base,
		scripts: map[string]jobTransactionScript{
			"a-quarantined.bin": {commitSettlement: FileQuarantined},
		},
	}
	revisions := &jobRevisionClient{
		opened:   make(map[catalog.FileID]OpenedRevision),
		failures: make(map[catalog.FileID]error),
	}
	for index, file := range []catalog.FileID{quarantinedFile, publishedFile} {
		descriptor := jobDescriptor(t, share, file, byte(184+index), 1)
		opened, err := NewOpenedRevision(transferID[content.LeaseID](byte(190+index)), descriptor)
		if err != nil {
			t.Fatal(err)
		}
		revisions.opened[file] = opened
	}
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share,
		SyntheticRoot: root,
		Rules:         rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 192,
					jobEntry(t, quarantinedFile, "a-quarantined.bin", 1),
					jobEntry(t, publishedFile, "b-published.bin", 1),
				),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions,
		Blocks:    scriptedRangeReader{},
		Output:    output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != JobCompletedWithErrors || result.Settlement.Kind() != JobPausedNeedsAttention ||
		result.SucceededFiles != 1 || result.TerminationCause != nil || result.SettlementFailure != nil {
		t.Fatalf("quarantine-isolated result = %+v", result)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "a-quarantined.bin" ||
		!errors.Is(result.Files[0].Cause, ErrOutputQuarantined) ||
		result.Files[0].Settlement.Kind() != FileQuarantined || result.Files[0].SettlementFailure != nil {
		t.Fatalf("quarantined file failure = %+v", result.Files)
	}
	if binding, ok := result.Files[0].Settlement.OutputBinding(); !ok ||
		binding != base.transactions["a-quarantined.bin"].Binding() {
		t.Fatalf("quarantine lost transaction binding: binding=%+v ok=%t", binding, ok)
	}
	if base.transactions["a-quarantined.bin"].commitCalls != 1 ||
		base.transactions["b-published.bin"].commitCalls != 1 ||
		!base.transactions["b-published.bin"].committed || base.pauseCalls != 0 || base.completeCalls != 1 {
		t.Fatalf("sibling flow = quarantined=%+v published=%+v pause=%d complete=%d",
			base.transactions["a-quarantined.bin"], base.transactions["b-published.bin"],
			base.pauseCalls, base.completeCalls)
	}
}
