package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3TransferJobPausesEveryInitialStateInstallOutcomeAndConverges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		fault           stateStoreFaultPoint
		wantFileRecords uint64
	}{
		{name: "not installed", fault: stateStoreFaultCreate, wantFileRecords: 0},
		{name: "adopted with cleanup failure", fault: stateStoreFaultCreateFixedClose, wantFileRecords: 1},
		{name: "uncertain after mutation", fault: stateStoreFaultCurrentReopen, wantFileRecords: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 17)
			base := v3RecoveryAuthority(t, root, nil)
			authority := &outputV3StateInstallJobAuthority{
				base: base, expected: selection, nextFault: test.fault,
			}
			blocks := &outputV3StateInstallJobBlocks{}

			first := outputV3StateInstallTransferJob(t, selection, authority, blocks).Run(context.Background())
			pauseCalls, completeCalls := authority.settlementCalls()
			if first.Outcome != transfer.JobPausedOutcome || pauseCalls != 1 || completeCalls != 0 {
				t.Fatalf("initial state-install job = (outcome=%v, pause=%d, complete=%d, termination=%v, settlement=%v)",
					first.Outcome, pauseCalls, completeCalls, first.TerminationCause, first.SettlementFailure)
			}
			var outputFailure *transfer.OutputFault
			if !errors.As(first.TerminationCause, &outputFailure) ||
				outputFailure.Scope() != transfer.OutputFaultFile || outputFailure.Code() != transfer.OutputFaultStateIO {
				t.Fatalf("initial state-install termination = %v", first.TerminationCause)
			}
			if calls := blocks.count(); calls != 0 {
				t.Fatalf("initial state-install failure transferred %d ranges", calls)
			}
			if err := authority.closeLatestSession(); err != nil {
				t.Fatal(err)
			}

			inventory, err := base.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			summaries := inventory.Summaries()
			if len(summaries) != 1 || summaries[0].Reference.ResumeIntent() != selection.ResumeIntent() ||
				summaries[0].FileRecords != test.wantFileRecords {
				_ = inventory.Close()
				t.Fatalf("retained state = %+v, want one canonical session with %d file records",
					summaries, test.wantFileRecords)
			}
			if err := inventory.Close(); err != nil {
				t.Fatal(err)
			}

			second := outputV3StateInstallTransferJob(t, selection, authority, blocks).Run(context.Background())
			pauseCalls, completeCalls = authority.settlementCalls()
			if second.Outcome != transfer.JobSucceeded || second.SucceededFiles != 1 ||
				pauseCalls != 1 || completeCalls != 1 || second.TerminationCause != nil || second.SettlementFailure != nil {
				t.Fatalf("restart state-install job = (outcome=%v, succeeded=%d, pause=%d, complete=%d, termination=%v, settlement=%v)",
					second.Outcome, second.SucceededFiles, pauseCalls, completeCalls,
					second.TerminationCause, second.SettlementFailure)
			}
			if calls := blocks.count(); calls != 1 {
				t.Fatalf("restart transferred %d ranges, want one total range without retransmission", calls)
			}
			contentBytes, err := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
			if err != nil || !bytes.Equal(contentBytes, bytes.Repeat([]byte{outputV3StateInstallJobByte}, 17)) {
				t.Fatalf("published restart content = (%x, %v)", contentBytes, err)
			}
		})
	}
}

func TestOutputV3TransferJobIsolatesPreStateCollisionAndCompletesRemainingFiles(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	const collisionPath = "collision.bin"
	const survivorPath = "survivor.bin"
	original := []byte("preexisting owner content")
	if err := os.WriteFile(filepath.Join(root, collisionPath), original, 0o600); err != nil {
		t.Fatal(err)
	}
	selection := v3RecoverySelectionPaths(t, []string{collisionPath, survivorPath}, 17)
	base := v3RecoveryAuthority(t, root, nil)
	authority := &outputV3StateInstallJobAuthority{base: base, expected: selection}
	blocks := &outputV3StateInstallJobBlocks{}

	result := outputV3StateInstallTransferJob(t, selection, authority, blocks).Run(context.Background())
	pauseCalls, completeCalls := authority.settlementCalls()
	if result.Outcome != transfer.JobCompletedWithErrors || result.SucceededFiles != 1 ||
		len(result.Files) != 1 || result.Files[0].Path != collisionPath ||
		result.Files[0].Settlement.Kind() != transfer.FileCollision ||
		pauseCalls != 0 || completeCalls != 1 || result.TerminationCause != nil || result.SettlementFailure != nil {
		t.Fatalf("isolated collision job = (outcome=%v, succeeded=%d, files=%+v, pause=%d, complete=%d, termination=%v, settlement=%v)",
			result.Outcome, result.SucceededFiles, result.Files, pauseCalls, completeCalls,
			result.TerminationCause, result.SettlementFailure)
	}
	if calls := blocks.count(); calls != 1 {
		t.Fatalf("isolated collision transferred %d ranges, want only the surviving file", calls)
	}
	collisionBytes, collisionErr := os.ReadFile(filepath.Join(root, collisionPath))
	survivorBytes, survivorErr := os.ReadFile(filepath.Join(root, survivorPath))
	if collisionErr != nil || !bytes.Equal(collisionBytes, original) ||
		survivorErr != nil || !bytes.Equal(survivorBytes, bytes.Repeat([]byte{outputV3StateInstallJobByte}, 17)) {
		t.Fatalf("isolated collision outputs = (collision=%x/%v, survivor=%x/%v)",
			collisionBytes, collisionErr, survivorBytes, survivorErr)
	}
	inventory, err := base.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("completed isolated-collision job retained resume state: %+v", summaries)
	}
}

const outputV3StateInstallJobByte = byte(0x6d)

type outputV3StateInstallJobPageCommitter struct{}

func (outputV3StateInstallJobPageCommitter) Commit(
	input catalog.PageCommitInput,
) (catalog.PageCommitment, error) {
	var commitment catalog.PageCommitment
	commitment[0] = input.DirectoryID.Bytes()[0]
	commitment[1] = byte(input.PageIndex + 1)
	commitment[2] = byte(len(input.Entries) + 1)
	return commitment, nil
}

type outputV3StateInstallJobCatalog struct {
	directory catalog.DirectoryID
	snapshot  catalog.DirectorySnapshot
}

func (source outputV3StateInstallJobCatalog) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	if err := ctx.Err(); err != nil {
		return catalog.DirectorySnapshot{}, func() {}, err
	}
	if directory != source.directory {
		return catalog.DirectorySnapshot{}, func() {}, fmt.Errorf("unexpected catalog directory %x", directory)
	}
	return source.snapshot, func() {}, nil
}

func (source outputV3StateInstallJobCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory != source.directory {
		return nil, fmt.Errorf("unexpected catalog directory %x", directory)
	}
	return &outputV3StateInstallPageCursor{snapshot: source.snapshot}, nil
}

type outputV3StateInstallPageCursor struct {
	snapshot catalog.DirectorySnapshot
	index    uint32
}

func (cursor *outputV3StateInstallPageCursor) Next(
	ctx context.Context,
) (catalog.CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return catalog.CatalogPage{}, false, err
	}
	page, found := cursor.snapshot.Page(cursor.index)
	if !found {
		return catalog.CatalogPage{}, false, nil
	}
	cursor.index++
	return page, true, nil
}

func (*outputV3StateInstallPageCursor) Close() error { return nil }

type outputV3StateInstallJobRevisions struct {
	opened map[catalog.FileID]transfer.OpenedRevision
}

func (client outputV3StateInstallJobRevisions) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
) (transfer.OpenedRevision, error) {
	if err := ctx.Err(); err != nil {
		return transfer.OpenedRevision{}, err
	}
	opened, found := client.opened[file]
	if !found {
		return transfer.OpenedRevision{}, fmt.Errorf("unexpected revision %x", file)
	}
	return opened, nil
}

func (outputV3StateInstallJobRevisions) ReleaseRevision(context.Context, content.LeaseID) error {
	return nil
}

type outputV3StateInstallJobBlocks struct {
	mu    sync.Mutex
	calls int
}

func (reader *outputV3StateInstallJobBlocks) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	sink transfer.RangeSink,
) error {
	reader.mu.Lock()
	reader.calls++
	reader.mu.Unlock()
	return sink.WriteRange(
		ctx, requested.Offset, bytes.Repeat([]byte{outputV3StateInstallJobByte}, int(requested.Length())),
	)
}

func (reader *outputV3StateInstallJobBlocks) count() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.calls
}

func outputV3StateInstallTransferJob(
	t *testing.T,
	selection transfer.OutputSelection,
	authority transfer.OutputAuthority,
	blocks transfer.RangeReader,
) *transfer.TransferJob {
	t.Helper()
	entries := make([]catalog.Entry, 0, len(selection.Files()))
	opened := make(map[catalog.FileID]transfer.OpenedRevision, len(selection.Files()))
	for index, selected := range selection.Files() {
		entry, err := catalog.NewFileEntry(
			selected.FileID, selected.Path, selected.ExpectedSize, selected.ModifiedTime,
		)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
		geometry, err := content.NewFileGeometry(selected.ExpectedSize, catalog.DefaultChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(
			selection.ShareInstance(), selected.FileID,
			v3RecoveryIdentity16[content.FileRevision](byte(90+index)), geometry, selected.ModifiedTime,
		)
		if err != nil {
			t.Fatal(err)
		}
		opened[selected.FileID], err = transfer.NewOpenedRevision(
			v3RecoveryIdentity16[content.LeaseID](byte(110+index)), descriptor,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: selection.ShareInstance(), DirectoryID: selection.SyntheticRoot(),
		Generation: selection.RootGeneration(), Entries: entries, Terminal: true,
	}, outputV3StateInstallJobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ShareInstance: selection.ShareInstance(), SyntheticRoot: selection.SyntheticRoot(), Rules: rules,
		Catalog: outputV3StateInstallJobCatalog{
			directory: selection.SyntheticRoot(), snapshot: snapshot,
		},
		Revisions: outputV3StateInstallJobRevisions{opened: opened}, Blocks: blocks, Output: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

type outputV3StateInstallJobAuthority struct {
	base      *Authority
	expected  transfer.OutputSelection
	nextFault stateStoreFaultPoint

	mu            sync.Mutex
	sessions      []*Session
	pauseCalls    int
	completeCalls int
}

func (authority *outputV3StateInstallJobAuthority) OpenSelection(
	ctx context.Context,
	selection transfer.OutputSelection,
) (transfer.OutputSession, error) {
	if selection.Identity() != authority.expected.Identity() ||
		selection.ResumeIntent() != authority.expected.ResumeIntent() {
		return nil, transfer.ErrInvalidOutputSelection
	}
	opened, err := authority.base.OpenSelection(ctx, selection)
	if err != nil {
		return nil, err
	}
	session, ok := opened.(*Session)
	if !ok {
		return nil, transfer.ErrInvalidOutputBinding
	}

	authority.mu.Lock()
	fault := authority.nextFault
	authority.nextFault = stateStoreFaultNone
	authority.sessions = append(authority.sessions, session)
	authority.mu.Unlock()
	if fault != stateStoreFaultNone {
		selected := selection.Files()[0]
		recordName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(selected.Path))
		shard, _, shardErr := openOutputShard(session.filesDir, recordName.Shard(), true)
		if shardErr != nil {
			_ = session.closeHandles()
			return nil, shardErr
		}
		if closeErr := shard.Close(); closeErr != nil {
			_ = session.closeHandles()
			return nil, closeErr
		}
		session.filesDir = &outputV3StateInstallJobShardParent{
			Directory: session.filesDir,
			shard:     recordName.Shard(),
			target:    recordName.Name(),
			fault:     fault,
		}
	}
	return &outputV3StateInstallObservedSession{OutputSession: session, authority: authority}, nil
}

func (authority *outputV3StateInstallJobAuthority) settlementCalls() (int, int) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.pauseCalls, authority.completeCalls
}

func (authority *outputV3StateInstallJobAuthority) closeLatestSession() error {
	authority.mu.Lock()
	if len(authority.sessions) == 0 {
		authority.mu.Unlock()
		return errors.New("state-install job did not open an output session")
	}
	session := authority.sessions[len(authority.sessions)-1]
	authority.mu.Unlock()
	return session.closeHandles()
}

type outputV3StateInstallObservedSession struct {
	transfer.OutputSession
	authority *outputV3StateInstallJobAuthority
}

func (session *outputV3StateInstallObservedSession) PauseJob(
	ctx context.Context,
	reason transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	session.authority.mu.Lock()
	session.authority.pauseCalls++
	session.authority.mu.Unlock()
	return session.OutputSession.PauseJob(ctx, reason)
}

func (session *outputV3StateInstallObservedSession) CompleteJob(
	ctx context.Context,
	outcome transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	session.authority.mu.Lock()
	session.authority.completeCalls++
	session.authority.mu.Unlock()
	return session.OutputSession.CompleteJob(ctx, outcome)
}

type outputV3StateInstallJobShardParent struct {
	outputcap.Directory
	shard  string
	target string
	fault  stateStoreFaultPoint
}

func (parent *outputV3StateInstallJobShardParent) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := parent.Directory.OpenDirectory(name, private)
	if err != nil || name != parent.shard {
		return opened, err
	}
	return &stateStoreFaultDirectory{
		Directory: opened, fault: parent.fault, target: parent.target,
	}, nil
}
