package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
)

// A session that ends because its peer closed it cancels its own lifetime. The
// receiver joins that cancellation into the diagnostics of an already classified
// session-terminal fault. The caller's context is still live, so the outcome
// must remain a session failure instead of becoming a caller interruption.
func TestTransferJobSessionTerminalWithLifetimeCancellationIsNotCallerInterruption(t *testing.T) {
	share := transferID[catalog.ShareInstance](100)
	root := transferID[catalog.DirectoryID](101)
	file := transferID[catalog.FileID](102)
	chunk := uint64(catalog.MinChunkSize)
	snapshot := multiPageJobSnapshot(t, share, root, 103, jobEntry(t, file, "file.bin", chunk))
	descriptor := jobDescriptor(t, share, file, 104, chunk)
	opened, _ := NewOpenedRevision(transferID[RevisionHandle](105), descriptor)
	revisions := &jobRevisionClient{opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error)}
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	terminalFault := mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport)
	terminal := fault.Wrap(terminalFault, errors.Join(
		errors.New("session runtime is closed"), context.Canceled, errors.New("peer ended the session"),
	))
	replayBlocked := make(chan struct{})
	close(replayBlocked)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: &sessionFailureReplayBarrierCatalog{snapshot: snapshot, replayBlocked: replayBlocked}, Revisions: revisions,
		Blocks: replayBarrierSessionFailingBlocks{replayBlocked: replayBlocked, err: terminal}, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused || result.TerminationInterruption != 0 ||
		result.TerminationFault != terminalFault {
		t.Fatalf("session terminal outcome = %v, interruption %v, fault %v; want paused, none, %v",
			result.Outcome, result.TerminationInterruption, result.TerminationFault, terminalFault)
	}
}
