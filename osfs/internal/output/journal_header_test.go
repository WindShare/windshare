//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReadJournalHeaderReadsOnlyImmutableFirstRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xd1)
	opID := intent.OperationID()

	// Append a lifecycle update and force compaction so line 1 is rewritten
	// through compactLocked, then verify the immutable header remains intact.
	if err := op.RecordShareStopped(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op.journal.mu.Lock()
	compactErr := op.journal.compactLocked()
	op.journal.mu.Unlock()
	if compactErr != nil {
		_ = auth.Close()
		t.Fatal(compactErr)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	opPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))
	journalPath := filepath.Join(opPath, journalFileName)

	// Append non-tail corrupt lines that cause full readJournalState to fail.
	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("deadbeef {\"k\":\"partial\",\"corrupt\":true}\nstill-corrupt\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	opDir, err := openDirHandle(opPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opDir.Close()

	if _, _, err := readJournalState(opDir); !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("readJournalState on corrupt body = %v, want errJournalCorrupt", err)
	}
	header, err := readJournalHeader(opDir)
	if err != nil {
		t.Fatalf("readJournalHeader = %v, want nil", err)
	}
	if !header.Intent.Equal(intent) ||
		header.SelectionDigest != intent.SelectionSpec().Digest() ||
		header.Redownload {
		t.Fatalf("readJournalHeader mismatch: got %+v, want intent=%x redownload=false", header, intent.Digest())
	}

	// Leading blank lines before a valid meta header remain tolerated.
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, append([]byte("\n\n"), raw...), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournalHeader(opDir); err != nil {
		t.Fatalf("readJournalHeader with leading blank lines = %v", err)
	}
}

func TestReadJournalHeaderRejectsMalformedInputs(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir, err := openDirHandle(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		if _, err := readJournalHeader(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing journal = %v, want fs.ErrNotExist", err)
		}
	})

	cases := []struct {
		name    string
		content []byte
	}{
		{name: "empty file", content: nil},
		{name: "blank lines only", content: []byte("\n\n")},
		{name: "bad crc", content: []byte("00000000 {\"k\":\"meta\"}\n")},
		{name: "non-meta first line", content: mustEncodeJournalLine(t, journalRecord{
			Kind: "retired", Path: "a.bin",
		})},
		{name: "lifecycle meta without intent", content: mustEncodeJournalLine(t, journalRecord{
			Kind: "meta", Lifecycle: journalLifecycleStopped, StateGen: 2,
		})},
		{name: "oversized line without newline", content: bytes.Repeat([]byte("a"), journalMaxLineBytes+64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			if err := os.WriteFile(filepath.Join(path, journalFileName), tc.content, privateFileMode); err != nil {
				t.Fatal(err)
			}
			dir, err := openDirHandle(path)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			if _, err := readJournalHeader(dir); !errors.Is(err, errJournalCorrupt) {
				t.Fatalf("readJournalHeader(%s) = %v, want errJournalCorrupt", tc.name, err)
			}
		})
	}
}

func TestAutomaticLookupAndSingleFileReservationUseHeaderOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Create a paused single-file task A claiming retirementFileName, then
	// corrupt its journal body (leaving line 1 intact). If lookup or single-file
	// reservation replayed the full journal, both would fail to see task A.
	selA, intentA, _ := createPausedSingleFileOp(t, root, 0xe1)
	journalA := filepath.Join(root, ControlDirName, hex.EncodeToString(intentA.OperationID().Bytes()), journalFileName)
	f, err := os.OpenFile(journalA, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("deadbeef {\"k\":\"delivered\",\"corrupt\":true}\nsecond-corrupt-line\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Also add an unrelated redownload single-file op claiming a distinct name
	// to verify redownload tasks stay excluded from automatic matching while
	// still protecting their reserved top-level filename.
	redlAuth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := redlAuth.BindDestination(ctx); err != nil {
		_ = redlAuth.Close()
		t.Fatal(err)
	}
	selB := testSelectionSpec(t, 0xe7)
	redlLookup, err := redlAuth.SelectOperation(ctx, selB, receivecontract.RedownloadOperation())
	if err != nil || redlLookup.Kind() != LookupCreateReady {
		_ = redlAuth.Close()
		t.Fatalf("redownload select = (%v, %v)", redlLookup.Kind(), err)
	}
	redlArt, err := receivecontract.NewSingleFileDirectoryTree(testID16[catalog.FileID](0xe9), retirementFileName, retirementFileName)
	if err != nil {
		_ = redlAuth.Close()
		t.Fatal(err)
	}
	redlOp, err := redlAuth.CreateOperation(ctx, redlLookup, redlArt)
	if err != nil {
		_ = redlAuth.Close()
		t.Fatal(err)
	}
	redlIntent, _ := redlOp.ReceiveIntent()
	redlRes, _ := redlIntent.MaterializationPlan().DestinationReservation()
	if redlRes.PhysicalName() == retirementFileName {
		_ = redlAuth.Close()
		t.Fatalf("redownload single-file op collided with task A's reserved name %q", retirementFileName)
	}
	if err := redlAuth.Close(); err != nil {
		t.Fatal(err)
	}

	// Start an unrelated automatic single-file download with the same suggested
	// name: it must neither match selB's redownload task nor collide with task
	// A's or the redownload task's reserved names.
	autoAuth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer autoAuth.Close()
	if err := autoAuth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookupB, err := autoAuth.SelectOperation(ctx, selB, receivecontract.AutomaticOperation())
	if err != nil || lookupB.Kind() != LookupCreateReady {
		t.Fatalf("unrelated automatic select = (%v, %v), want LookupCreateReady", lookupB.Kind(), err)
	}
	opB, err := autoAuth.CreateOperation(ctx, lookupB, redlArt)
	if err != nil {
		t.Fatalf("unrelated single-file create = %v", err)
	}
	intentB, _ := opB.ReceiveIntent()
	resB, _ := intentB.MaterializationPlan().DestinationReservation()
	if resB.PhysicalName() == retirementFileName || resB.PhysicalName() == redlRes.PhysicalName() {
		t.Fatalf("unrelated single-file op collided with existing claims: got %q, existing {%q, %q}",
			resB.PhysicalName(), retirementFileName, redlRes.PhysicalName())
	}

	// Reopening selA matches task A from its header and defers full replay to
	// openTaskJournal, surfacing LookupNeedsAttention instead of silently
	// ignoring the damaged task and creating a duplicate.
	reopenA, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopenA.Close()
	if err := reopenA.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookupA, err := reopenA.SelectOperation(ctx, selA, receivecontract.AutomaticOperation())
	if err != nil || lookupA.Kind() != LookupNeedsAttention || lookupA.Attention() != AttentionOperation {
		t.Fatalf("reopen damaged task A = (kind=%v, attn=%q, err=%v), want LookupNeedsAttention(%q)",
			lookupA.Kind(), lookupA.Attention(), err, AttentionOperation)
	}
}

func mustEncodeJournalLine(t *testing.T, rec journalRecord) []byte {
	t.Helper()
	line, err := encodeJournalLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestReopenReleasesDirLockBeforeJournalReplay(t *testing.T) {
	ctx := context.Background()
	const waitTimeout = 5 * time.Second

	cases := []struct {
		name string
		seed byte
		run  func(t *testing.T, root string, hook func(*Dir, func() time.Time) (*taskJournal, error)) (
			opID receivecontract.OperationID,
			selector receivecontract.OperationSelection,
			reopen func() (receivecontract.OperationID, error),
			cleanup func() error,
		)
	}{
		{
			name: "automatic select",
			seed: 0xf1,
			run: func(t *testing.T, root string, hook func(*Dir, func() time.Time) (*taskJournal, error)) (
				receivecontract.OperationID, receivecontract.OperationSelection, func() (receivecontract.OperationID, error), func() error,
			) {
				sel, intent, _ := createPausedSingleFileOp(t, root, 0xf1)
				auth := newBoundAuthority(t, root)
				auth.openJournal = hook
				selector := receivecontract.AutomaticOperation()
				return intent.OperationID(), selector, func() (receivecontract.OperationID, error) {
					lookup, err := auth.SelectOperation(ctx, sel, selector)
					if err != nil {
						return receivecontract.OperationID{}, err
					}
					if lookup.Kind() != LookupReopened || lookup.Operation() == nil {
						return receivecontract.OperationID{}, errors.New("expected LookupReopened")
					}
					gotIntent, _ := lookup.Operation().ReceiveIntent()
					return gotIntent.OperationID(), nil
				}, auth.Close
			},
		},
		{
			name: "explicit by id",
			seed: 0xf4,
			run: func(t *testing.T, root string, hook func(*Dir, func() time.Time) (*taskJournal, error)) (
				receivecontract.OperationID, receivecontract.OperationSelection, func() (receivecontract.OperationID, error), func() error,
			) {
				sel, intent, _ := createPausedSingleFileOp(t, root, 0xf4)
				auth := newBoundAuthority(t, root)
				auth.openJournal = hook
				selector, err := receivecontract.SelectOperationID(intent.OperationID())
				if err != nil {
					_ = auth.Close()
					t.Fatal(err)
				}
				return intent.OperationID(), selector, func() (receivecontract.OperationID, error) {
					lookup, err := auth.SelectOperation(ctx, sel, selector)
					if err != nil {
						return receivecontract.OperationID{}, err
					}
					if lookup.Kind() != LookupReopened || lookup.Operation() == nil {
						return receivecontract.OperationID{}, errors.New("expected LookupReopened")
					}
					gotIntent, _ := lookup.Operation().ReceiveIntent()
					return gotIntent.OperationID(), nil
				}, auth.Close
			},
		},
		{
			name: "create recheck",
			seed: 0xf7,
			run: func(t *testing.T, root string, hook func(*Dir, func() time.Time) (*taskJournal, error)) (
				receivecontract.OperationID, receivecontract.OperationSelection, func() (receivecontract.OperationID, error), func() error,
			) {
				sel := testSelectionSpec(t, 0xf7)
				auth, lookup := prepareCreateReadyAuthority(t, root, sel, receivecontract.AutomaticOperation())
				auth.openJournal = hook

				// Another instance creates and pauses the matching operation before
				// auth reaches CreateOperation's automatic uniqueness re-check.
				_, intent, fileID := createPausedSingleFileOp(t, root, 0xf7)
				art, err := receivecontract.NewSingleFileDirectoryTree(fileID, retirementFileName, retirementFileName)
				if err != nil {
					_ = auth.Close()
					t.Fatal(err)
				}
				return intent.OperationID(), receivecontract.AutomaticOperation(), func() (receivecontract.OperationID, error) {
					op, err := auth.CreateOperation(ctx, lookup, art)
					if err != nil {
						return receivecontract.OperationID{}, err
					}
					gotIntent, _ := op.ReceiveIntent()
					return gotIntent.OperationID(), nil
				}, auth.Close
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			entered := make(chan struct{})
			release := make(chan struct{})
			hook := func(opDir *Dir, now func() time.Time) (*taskJournal, error) {
				close(entered)
				<-release
				return openTaskJournal(opDir, now)
			}

			wantOpID, selector, runReopen, cleanup := tc.run(t, root, hook)
			defer func() {
				if err := cleanup(); err != nil {
					t.Fatalf("close reopen authority: %v", err)
				}
			}()

			type reopenOutcome struct {
				opID receivecontract.OperationID
				err  error
			}
			done := make(chan reopenOutcome, 1)
			go func() {
				gotID, err := runReopen()
				done <- reopenOutcome{opID: gotID, err: err}
			}()

			select {
			case <-entered:
			case out := <-done:
				t.Fatalf("reopen finished before entering journal replay: (%x, %v)", out.opID, out.err)
			case <-time.After(waitTimeout):
				t.Fatal("timed out waiting for journal replay hook")
			}

			// While journal replay is paused, dirLock must be free so an unrelated
			// operation in the same destination can select and create without blocking.
			unrelatedDone := make(chan error, 1)
			go func() {
				otherAuth := newBoundAuthority(t, root)
				defer otherAuth.Close()
				otherSel := testSelectionSpec(t, tc.seed+0x20)
				otherLookup, err := otherAuth.SelectOperation(ctx, otherSel, receivecontract.AutomaticOperation())
				if err != nil || otherLookup.Kind() != LookupCreateReady {
					unrelatedDone <- errors.Join(err, errors.New("unrelated select failed"))
					return
				}
				otherArt, err := receivecontract.NewSingleFileDirectoryTree(
					testID16[catalog.FileID](tc.seed+0x22), "unrelated.bin", "unrelated.bin",
				)
				if err != nil {
					unrelatedDone <- err
					return
				}
				if _, err := otherAuth.CreateOperation(ctx, otherLookup, otherArt); err != nil {
					unrelatedDone <- err
					return
				}
				unrelatedDone <- nil
			}()

			select {
			case err := <-unrelatedDone:
				if err != nil {
					close(release)
					<-done
					t.Fatalf("unrelated operation blocked or failed during journal replay: %v", err)
				}
			case <-time.After(waitTimeout):
				close(release)
				<-done
				t.Fatal("unrelated operation blocked on dirLock while reopen replayed state.log")
			}

			// The claimed task itself must stay protected by task.lock: concurrent
			// discard sees ErrBusy and a second reopen sees LookupAlreadyRunning.
			if _, err := DiscardOperation(ctx, root, wantOpID); !errors.Is(err, ErrBusy) {
				close(release)
				<-done
				t.Fatalf("DiscardOperation during journal replay = %v, want ErrBusy", err)
			}
			contender := newBoundAuthority(t, root)
			contendLookup, contendErr := contender.SelectOperation(ctx, testSelectionSpec(t, tc.seed), selector)
			_ = contender.Close()
			if contendErr != nil || contendLookup.Kind() != LookupAlreadyRunning {
				close(release)
				<-done
				t.Fatalf("concurrent select during journal replay = (kind=%v, err=%v), want LookupAlreadyRunning",
					contendLookup.Kind(), contendErr)
			}

			close(release)
			select {
			case out := <-done:
				if out.err != nil || out.opID != wantOpID {
					t.Fatalf("reopen outcome = (%x, %v), want %x", out.opID, out.err, wantOpID)
				}
			case <-time.After(waitTimeout):
				t.Fatal("timed out waiting for reopen completion")
			}
		})
	}

	t.Run("rejected journal releases task lock for subsequent discard", func(t *testing.T) {
		root := t.TempDir()
		sel, intent, _ := createPausedSingleFileOp(t, root, 0xfa)
		opID := intent.OperationID()

		journalPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()), journalFileName)
		f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, privateFileMode)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("deadbeef {\"k\":\"partial\",\"bad\":true}\nsecond-corrupt\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		entered := make(chan struct{})
		release := make(chan struct{})
		auth := newBoundAuthority(t, root)
		defer auth.Close()
		auth.openJournal = func(opDir *Dir, now func() time.Time) (*taskJournal, error) {
			close(entered)
			<-release
			return openTaskJournal(opDir, now)
		}

		type selectOutcome struct {
			lookup ActiveLookup
			err    error
		}
		done := make(chan selectOutcome, 1)
		go func() {
			lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
			done <- selectOutcome{lookup: lookup, err: err}
		}()

		select {
		case <-entered:
		case <-time.After(waitTimeout):
			t.Fatal("timed out waiting for corrupt journal replay")
		}
		if _, err := DiscardOperation(ctx, root, opID); !errors.Is(err, ErrBusy) {
			close(release)
			<-done
			t.Fatalf("DiscardOperation while reading corrupt journal = %v, want ErrBusy", err)
		}
		close(release)
		select {
		case out := <-done:
			if out.err != nil || out.lookup.Kind() != LookupNeedsAttention || out.lookup.Attention() != AttentionOperation {
				t.Fatalf("corrupt reopen = (kind=%v, attn=%q, err=%v), want LookupNeedsAttention(%q)",
					out.lookup.Kind(), out.lookup.Attention(), out.err, AttentionOperation)
			}
		case <-time.After(waitTimeout):
			t.Fatal("timed out waiting for corrupt reopen result")
		}

		summary, err := DiscardOperation(ctx, root, opID)
		if err != nil || summary.State() != RecoveryOperationDiscarded {
			t.Fatalf("DiscardOperation after rejected reopen = (%+v, %v), want discarded", summary, err)
		}
	})
}

func TestListOperationsUsesHeaderOnlyForRunningConflictedOperation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0xf1)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = auth.Close()
		}
	})

	fileID := testID16[catalog.FileID](0xf3)
	initial := testMaterializationFileWithIDs(
		t, session, intent, fileID, testID16[content.FileRevision](0xf4), "drift.bin", 8, rootAdm,
	)
	start, err := session.BeginFile(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected initial file transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if paused, err := tx.Pause(ctx); err != nil || paused.Kind() != transfer.FilePaused {
		t.Fatalf("pause = (%v, %v)", paused.Kind(), err)
	}

	changed := testMaterializationFileWithIDs(
		t, session, intent, fileID, testID16[content.FileRevision](0xf5), "drift.bin", 12, rootAdm,
	)
	blockedStart, err := session.BeginFile(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := blockedStart.ImmediateSettlement()
	if !immediate || settled.Kind() != transfer.FileItemBlocked {
		t.Fatalf("changed revision start = (immediate=%t, kind=%v), want FileItemBlocked", immediate, settled.Kind())
	}

	// While task.lock is held, ListOperations must read only the header and
	// omit per-item checkpoint details including blocked items.
	runningInv, err := ListOperations(ctx, root)
	if err != nil || runningInv.Status() != RecoveryListReady || runningInv.UnknownEntries() || len(runningInv.Summaries()) != 1 {
		t.Fatalf("running inventory = (%+v, %v), want ready with 1 summary", runningInv, err)
	}
	runningSummary := runningInv.Summaries()[0]
	if !runningSummary.Valid() || !runningSummary.Busy() ||
		runningSummary.OperationID() != intent.OperationID() ||
		runningSummary.ReceiveIntentDigest() != intent.Digest() ||
		runningSummary.StateGeneration() != initialStateGeneration ||
		runningSummary.State() != RecoveryOperationIncomplete ||
		runningSummary.NeedsAttentionReason() != AttentionNone ||
		len(runningSummary.Items()) != 0 {
		t.Fatalf("running summary = %+v, want busy incomplete without items", runningSummary)
	}

	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	// Once task.lock is released, full journal inspection surfaces the recorded conflict.
	idleInv, err := ListOperations(ctx, root)
	if err != nil || idleInv.Status() != RecoveryListReady || idleInv.UnknownEntries() || len(idleInv.Summaries()) != 1 {
		t.Fatalf("idle inventory = (%+v, %v), want ready with 1 summary", idleInv, err)
	}
	idleSummary := idleInv.Summaries()[0]
	items := idleSummary.Items()
	wantPath := changed.MaterializationRelativePath().String()
	if idleSummary.Busy() || len(items) != 1 ||
		items[0].CanonicalPath() != wantPath ||
		items[0].State() != RecoveryItemBlocked ||
		items[0].BlockReason() != RecoveryItemBlockRevisionConflict {
		t.Fatalf("idle summary = %+v items=%+v, want non-busy revision-conflict item %q", idleSummary, items, wantPath)
	}
}

func newBoundAuthority(t *testing.T, root string) *Authority {
	t.Helper()
	auth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(context.Background()); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	return auth
}
