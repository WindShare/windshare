package transfer

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

type pathScriptedJobOutput struct {
	*jobOutput
	scripts map[string]jobTransactionScript
}

func (output *pathScriptedJobOutput) OpenDirectTree(
	ctx context.Context,
	intent ReceiveIntent,
) (DirectTreeSession, error) {
	if _, err := output.jobOutput.OpenDirectTree(ctx, intent); err != nil {
		return nil, err
	}
	return output, nil
}

func (output *pathScriptedJobOutput) AdmitDirectory(
	ctx context.Context,
	directory MaterializationDirectory,
) (DirectoryAdmission, error) {
	return output.jobOutput.AdmitDirectory(ctx, directory)
}

func (output *pathScriptedJobOutput) BeginFile(
	ctx context.Context,
	file MaterializationFile,
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
	base.completeSettlement = DirectTreeSettlementNeedsAttention
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
	job, err := newTestTransferJob(t, testTransferJobConfig{
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
		Revisions:    revisions,
		Blocks:       scriptedRangeReader{},
		Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeNeedsAttention || result.Settlement.Kind() != DirectTreeSettlementNeedsAttention ||
		result.SucceededFiles != 1 || result.TerminationCause != nil || result.SettlementFailure != nil {
		t.Fatalf("quarantine-isolated result = %+v", result)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "a-quarantined.bin" ||
		!errors.Is(result.Files[0].Cause, ErrOutputQuarantined) ||
		result.Files[0].Settlement.Kind() != FileQuarantined || result.Files[0].SettlementFailure != nil {
		t.Fatalf("quarantined file failure = %+v", result.Files)
	}
	if binding, ok := result.Files[0].Settlement.MaterializedBinding(); !ok ||
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

func TestTransferJobStopsSiblingWorkOnUnsettledCommit(t *testing.T) {
	for _, test := range []struct {
		name  string
		scope fault.Scope
	}{
		{name: "file", scope: fault.ScopeFileLocal},
		{name: "output pause", scope: fault.ScopeOutputPause},
		{name: "session terminal", scope: fault.ScopeSessionTerminal},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := test.scope
			share := transferID[catalog.ShareInstance](200 + byte(scope))
			root := transferID[catalog.DirectoryID](204 + byte(scope))
			first := transferID[catalog.FileID](208 + byte(scope))
			second := transferID[catalog.FileID](212 + byte(scope))
			cause := errors.New("durable publication authority was lost")
			base := newJobOutput(share)
			output := &pathScriptedJobOutput{
				jobOutput: base,
				scripts: map[string]jobTransactionScript{
					"a-first.bin": {commitErr: outputFailure(scope, fault.OutputStateIO, cause)},
				},
			}
			revisions := &jobRevisionClient{
				opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
			}
			for index, file := range []catalog.FileID{first, second} {
				descriptor := jobDescriptor(t, share, file, byte(220+index), 0)
				revisions.opened[file], _ = NewOpenedRevision(
					transferID[content.LeaseID](byte(224+index)), descriptor,
				)
			}
			rules, _ := NewSelectionRules(true, nil)
			job, err := newTestTransferJob(t, testTransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
						root: jobSnapshot(t, share, root, 1,
							jobEntry(t, first, "a-first.bin", 0), jobEntry(t, second, "b-second.bin", 0),
						),
					},
					failures: make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
			})
			if err != nil {
				t.Fatal(err)
			}

			result := job.Run(context.Background())
			expected, _ := fault.NewOutput(scope, fault.OutputStateIO)
			if result.Outcome != DirectTreeOutcomeResumable || normalizedFault(result.TerminationCause) != expected ||
				normalizedFault(result.SettlementFailure) != expected ||
				!slices.Equal(revisions.order, []catalog.FileID{first}) ||
				base.transactions["a-first.bin"] == nil || base.transactions["b-second.bin"] != nil ||
				len(base.finalized) != 0 || base.pauseCalls != 1 || base.completeCalls != 0 {
				t.Fatalf("result=%+v revisions=%v transactions=%v finalized=%v pause=%d complete=%d",
					result, revisions.order, base.transactions, base.finalized, base.pauseCalls, base.completeCalls)
			}
		})
	}
}
