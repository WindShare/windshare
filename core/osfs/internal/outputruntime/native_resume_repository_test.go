package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type ordinaryResumeSessionFixture struct {
	authority     *Authority
	session       transfer.DirectTreeSession
	intent        transfer.ReceiveIntent
	transaction   transfer.FileTransaction
	durable       transfer.VerifiedDurableRanges
	settlement    transfer.FileSettlement
	rootAdmission transfer.DirectoryAdmission
	finalPath     string
}

func ordinaryResumeMaterializationFile(
	t *testing.T,
	session transfer.DirectTreeSession,
	projector ordinaryoutput.ArtifactPathProjector,
	descriptor content.FileRevisionDescriptor,
	path string,
	parent transfer.DirectoryAdmission,
	parentClaim transfer.MaterializedDirectoryClaim,
) transfer.MaterializationFile {
	t.Helper()
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(
		projector, sourcePath, descriptor, session.SessionID(), parent, parentClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func openOrdinaryResumeSession(
	t *testing.T,
	root string,
	seed byte,
	exactSize uint64,
) ordinaryResumeSessionFixture {
	t.Helper()
	fixture := openOrdinaryResumeFile(t, root, seed, exactSize, false)
	if fixture.transaction == nil {
		t.Fatal("ordinary file settled before content")
	}
	return fixture
}

func reopenOrdinaryResumeFile(
	t *testing.T,
	root string,
	seed byte,
	exactSize uint64,
) ordinaryResumeSessionFixture {
	t.Helper()
	return openOrdinaryResumeFile(t, root, seed, exactSize, true)
}

func openOrdinaryResumeFile(
	t *testing.T,
	root string,
	seed byte,
	exactSize uint64,
	reopen bool,
) ordinaryResumeSessionFixture {
	t.Helper()
	fileID := incrementalTestIdentity16[catalog.FileID](seed + 1)
	rules, err := transfer.NewSelectionRules(false, []transfer.SelectionOverride{{
		FileID: fileID, Selected: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		incrementalTestIdentity16[catalog.ShareInstance](seed),
		incrementalTestIdentity16[catalog.DirectoryID](seed+2),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	name := "resume.bin"
	artifact, err := receivecontract.NewSingleFileDirectoryTree(fileID, name, name)
	if err != nil {
		t.Fatal(err)
	}
	authority := newNativeReservationTestAuthority(t, root)
	mode, err := authority.BindDestination(context.Background())
	if err != nil || !mode.Resumable() {
		t.Fatalf("bind = %+v, %v", mode, err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	var operation *Operation
	if reopen {
		if lookup.Kind() != ActiveLookupReopened {
			t.Fatalf("reopen lookup = %d", lookup.Kind())
		}
		operation = lookup.Operation()
	} else {
		if lookup.Kind() != ActiveLookupMiss {
			t.Fatalf("lookup = %d", lookup.Kind())
		}
		operation, err = authority.CreateOperation(context.Background(), lookup, artifact)
		if err != nil {
			t.Fatal(err)
		}
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("ordinary operation omitted intent")
	}
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	session, err := authority.OpenOperation(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := transfer.OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	rootSource := transfer.AuthenticatedSourceDirectory{
		DirectoryID: selection.SyntheticRoot(),
		Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](seed + 2),
		SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
	}
	rootRequest, err := transfer.NewDirectoryMaterializationRequest(
		projector, rootSource, ordinaryoutput.SourceNodeConnectsSelection,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), rootRequest)
	if err != nil {
		t.Fatal(err)
	}
	geometry, err := content.NewFileGeometry(exactSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), fileID,
		incrementalTestIdentity16[content.FileRevision](seed+3),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	file := ordinaryResumeMaterializationFile(
		t, session, projector, descriptor, name, rootAdmission,
		transfer.MaterializedDirectoryClaim{},
	)
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, hasTransaction := start.Transaction()
	settlement, hasSettlement := start.ImmediateSettlement()
	if hasTransaction == hasSettlement {
		t.Fatal("ordinary file start did not return exactly one outcome")
	}
	return ordinaryResumeSessionFixture{
		authority: authority, session: session, intent: intent,
		transaction: transaction, durable: durable, settlement: settlement,
		rootAdmission: rootAdmission,
		finalPath:     filepath.Join(root, reservation.ReservedName()),
	}
}
func pauseOrdinaryResumeFixture(t *testing.T, fixture ordinaryResumeSessionFixture) {
	t.Helper()
	if settlement, err := fixture.transaction.Pause(
		context.Background(), transfer.FilePauseInterrupted,
	); err != nil || settlement.Kind() != transfer.FilePaused {
		t.Fatalf("pause file = %d, %v", settlement.Kind(), err)
	}
	if settlement, err := fixture.session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("pause tree = %d, %v", settlement.Kind(), err)
	}
	if err := fixture.authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeRepositoryPagesOrdinaryStateWithoutCreatingIt(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	page, err := repository.Page(
		context.Background(), resumeauthority.PageCursor{}, 16,
	)
	if err != nil || len(page.Headers()) != 0 {
		t.Fatalf("empty page = %d, %v", len(page.Headers()), err)
	}
	if _, err := os.Lstat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only inventory created control state: %v", err)
	}

	fixture := openOrdinaryResumeSession(t, root, 0x31, 4)
	if _, err := repository.Acquire(
		context.Background(), fixture.intent.OperationID(),
	); !errors.Is(err, ErrNativeResumeBusy) {
		t.Fatalf("busy acquire = %v", err)
	}
	page, err = repository.Page(
		context.Background(), resumeauthority.PageCursor{}, 16,
	)
	if err != nil || len(page.Headers()) != 1 ||
		page.Headers()[0].Record().OperationID() != fixture.intent.OperationID() {
		t.Fatalf("active page = %+v, %v", page.Headers(), err)
	}
	pauseOrdinaryResumeFixture(t, fixture)
}

func TestNativeResumeSnapshotClassifiesPartialAndPublishedFiles(t *testing.T) {
	for name, publish := range map[string]bool{"partial": false, "published": true} {
		t.Run(name, func(t *testing.T) {
			root := newRuntimeTestRootSpec(t).path
			fixture := openOrdinaryResumeSession(t, root, 0x41, 4)
			if err := fixture.transaction.WriteRange(
				context.Background(), 0, []byte("data"),
			); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
				t.Fatal(err)
			}
			if publish {
				settlement, err := fixture.transaction.Commit(context.Background())
				if err != nil || settlement.Kind() != transfer.FilePublished {
					t.Fatalf("commit = %d, %v", settlement.Kind(), err)
				}
				if info, err := os.Stat(fixture.finalPath); err != nil || info.Size() != 4 {
					t.Fatalf("final = %+v, %v", info, err)
				}
				if settlement, err := fixture.session.PauseTree(
					context.Background(), transfer.JobPauseInterrupted,
				); err != nil || settlement.Kind() != transfer.DirectTreeSettlementPaused {
					t.Fatalf("pause tree = %d, %v", settlement.Kind(), err)
				}
				if err := fixture.authority.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				pauseOrdinaryResumeFixture(t, fixture)
			}

			repository, _ := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
			lease, err := repository.Acquire(
				context.Background(), fixture.intent.OperationID(),
			)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := lease.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			items := snapshot.Items()
			if len(items) != 1 {
				t.Fatalf("items = %+v", items)
			}
			want := resumeauthority.ItemResumable
			if publish {
				want = resumeauthority.ItemPublished
			}
			if items[0].State() != want {
				t.Fatalf("item state = %s, want %s", items[0].State(), want)
			}

			authority, _ := resumeauthority.New(repository)
			result, err := authority.Discard(
				context.Background(), fixture.intent.OperationID(),
			)
			if err != nil || result.State() != resumeauthority.OperationDiscarded {
				t.Fatalf("discard = %s, %v", result.State(), err)
			}
			page, err := repository.Page(
				context.Background(), resumeauthority.PageCursor{}, 16,
			)
			if err != nil || len(page.Headers()) != 0 {
				t.Fatalf("post-discard page = %d, %v", len(page.Headers()), err)
			}
			_, finalErr := os.Stat(fixture.finalPath)
			if publish && finalErr != nil {
				t.Fatalf("discard removed published final: %v", finalErr)
			}
			if !publish && !errors.Is(finalErr, os.ErrNotExist) {
				t.Fatalf("discard created or adopted final: %v", finalErr)
			}
		})
	}
}

func TestNativeResumeCollisionRemainsResumableAndRetriesSameOperation(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x49, 4)
	if err := fixture.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)
	if err := os.WriteFile(fixture.finalPath, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := acquired.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot.Items()
	if snapshot.Header().Record().Lifecycle() != checkpointmodel.OrdinaryOperationActive ||
		len(items) != 1 || items[0].State() != resumeauthority.ItemResumable ||
		items[0].BlockReason() != resumeauthority.ItemBlockNone {
		t.Fatalf("collision inventory = lifecycle %s items %+v",
			snapshot.Header().Record().Lifecycle(), items)
	}
	if err := acquired.Close(); err != nil {
		t.Fatal(err)
	}

	collided := reopenOrdinaryResumeFile(t, root, 0x49, 4)
	if collided.intent.OperationID() != fixture.intent.OperationID() || collided.transaction != nil ||
		collided.settlement.Kind() != transfer.FileCollision {
		t.Fatalf("collision retry = operation %x transaction %T settlement %d",
			collided.intent.OperationID().Bytes(), collided.transaction, collided.settlement.Kind())
	}
	foreign, err := os.ReadFile(fixture.finalPath)
	if err != nil || string(foreign) != "foreign" {
		t.Fatalf("foreign final = %q, %v", foreign, err)
	}
	if tree, err := collided.session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || tree.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("collision pause = %d, %v", tree.Kind(), err)
	}
	if err := collided.authority.Close(); err != nil {
		t.Fatal(err)
	}
	postCollisionLease, err := repository.Acquire(
		context.Background(), fixture.intent.OperationID(),
	)
	if err != nil {
		t.Fatal(err)
	}
	postCollision, err := postCollisionLease.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	postCollisionItems := postCollision.Items()
	if postCollision.Header().Record().Lifecycle() != checkpointmodel.OrdinaryOperationActive ||
		len(postCollisionItems) != 1 ||
		postCollisionItems[0].State() != resumeauthority.ItemResumable ||
		postCollisionItems[0].BlockReason() != resumeauthority.ItemBlockNone {
		t.Fatalf("post-collision inventory = lifecycle %s items %+v",
			postCollision.Header().Record().Lifecycle(), postCollisionItems)
	}
	if err := postCollisionLease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.finalPath); err != nil {
		t.Fatal(err)
	}

	retry := reopenOrdinaryResumeFile(t, root, 0x49, 4)
	if retry.intent.OperationID() != fixture.intent.OperationID() || retry.transaction == nil {
		t.Fatalf("same-operation retry = operation %x transaction %T settlement %d",
			retry.intent.OperationID().Bytes(), retry.transaction, retry.settlement.Kind())
	}
	settlement, err := retry.transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("retry commit = %d, %v", settlement.Kind(), err)
	}
	published, err := os.ReadFile(fixture.finalPath)
	if err != nil || string(published) != "data" {
		t.Fatalf("published final = %q, %v", published, err)
	}
	if tree, err := retry.session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || tree.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("published pause = %d, %v", tree.Kind(), err)
	}
	if err := retry.authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeDiscardPreservesUnknownPartialAndBecomesCleanupPending(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x51, 4)
	if err := fixture.transaction.WriteRange(
		context.Background(), 0, []byte("data"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	operationPath := filepath.Join(
		root, checkpointstore.ControlDirectory,
		checkpointstore.OrdinaryRegistryDirectoryV1, "operations",
		filepath.Base(filepath.Join("", bytesToHex(fixture.intent.OperationID().Bytes()))),
		"files", checkpointstore.CheckpointsDirectory, checkpointstore.RecordsDirectory,
	)
	foreign := filepath.Join(operationPath, "foreign")
	if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	repository, _ := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	authority, _ := resumeauthority.New(repository)
	result, err := authority.Discard(
		context.Background(), fixture.intent.OperationID(),
	)
	if err != nil || result.State() != resumeauthority.OperationCleanupPending {
		t.Fatalf("discard = %s, %v", result.State(), err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "preserve" {
		t.Fatalf("foreign partial changed = %q, %v", data, err)
	}
	inventory, err := authority.List(context.Background())
	if err != nil || len(inventory.Summaries()) != 1 ||
		inventory.Summaries()[0].State() != resumeauthority.OperationCleanupPending {
		t.Fatalf("cleanup inventory = %+v, %v", inventory.Summaries(), err)
	}
	if _, err := os.Stat(fixture.finalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("discard published a final: %v", err)
	}
}

func TestNativeResumeLeaseSeparatesAttentionDiscardAndCleanup(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x61, 4)
	if err := fixture.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	lease, ok := acquired.(*NativeResumeLease)
	if !ok {
		t.Fatalf("native lease type = %T", acquired)
	}
	if state, err := lease.Cleanup(context.Background()); err == nil || state != 0 {
		t.Fatalf("active cleanup = (%d, %v)", state, err)
	}
	header, err := lease.Transition(
		context.Background(),
		checkpointmodel.OrdinaryLifecycleRequireAttention,
		checkpointmodel.OrdinaryReasonOperationOwnershipUnknown,
	)
	if err != nil || header.Record().Lifecycle() != checkpointmodel.OrdinaryOperationNeedsAttention {
		t.Fatalf("attention transition = (%+v, %v)", header.Record(), err)
	}
	snapshot, err := lease.Snapshot(context.Background())
	if err != nil || snapshot.Header().Record().Lifecycle() != checkpointmodel.OrdinaryOperationNeedsAttention ||
		len(snapshot.Items()) != 1 {
		t.Fatalf("attention snapshot = (%+v, %+v, %v)", snapshot.Header().Record(), snapshot.Items(), err)
	}
	header, err = lease.Transition(
		context.Background(),
		checkpointmodel.OrdinaryLifecycleDiscard,
		checkpointmodel.OrdinaryReasonNone,
	)
	if err != nil || header.Record().Lifecycle() != checkpointmodel.OrdinaryOperationDiscarded {
		t.Fatalf("discard transition = (%+v, %v)", header.Record(), err)
	}
	snapshot, err = lease.Snapshot(context.Background())
	if err != nil || len(snapshot.Items()) != 0 {
		t.Fatalf("terminal snapshot = (%+v, %v)", snapshot.Items(), err)
	}
	state, err := lease.Cleanup(context.Background())
	if err != nil || state != resumeauthority.CleanupComplete {
		t.Fatalf("terminal cleanup = (%d, %v)", state, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Snapshot(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("closed snapshot error = %v", err)
	}
	page, err := repository.Page(context.Background(), resumeauthority.PageCursor{}, 8)
	if err != nil || len(page.Headers()) != 0 {
		t.Fatalf("post-cleanup page = (%d, %v)", len(page.Headers()), err)
	}
	if _, err := os.Stat(fixture.finalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("discard changed final authority: %v", err)
	}
}

func TestNativeResumeRepositoryAndItemGuardsRejectUntrustedInputs(t *testing.T) {
	if _, err := NewNativeResumeRepository("relative", openOutputRuntimeTestPlatform); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative repository error = %v", err)
	}
	if _, err := NewNativeResumeRepository(filepath.Clean(t.TempDir()), nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil factory error = %v", err)
	}
	var nilRepository *NativeResumeRepository
	if _, err := nilRepository.Page(context.Background(), resumeauthority.PageCursor{}, 1); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil page error = %v", err)
	}
	if _, err := nilRepository.Acquire(context.Background(), receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil acquire error = %v", err)
	}

	root := newRuntimeTestRootSpec(t).path
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.Page(canceled, resumeauthority.PageCursor{}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled page error = %v", err)
	}
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat(
		[]byte{1}, receivecontract.StableIdentityBytes,
	))
	if _, err := repository.Acquire(canceled, operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v", err)
	}
	if _, err := repository.Acquire(context.Background(), operation); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent operation error = %v", err)
	}
	if closeNativeResumePlatform(nil) != nil ||
		closeNativeResumeRuntime(nil) != nil ||
		closeNativeResumeDirectory(nil) != nil ||
		closeNativeResumeFile(nil) != nil ||
		closeNativeResumeEntry(nil) != nil ||
		closeNativeResumeOwnedFile(nil) != nil {
		t.Fatal("nil native resume cleanup was not inert")
	}
	if _, err := ordinaryResumeRootDisposition(transfer.ReceiveIntent{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero resume intent error = %v", err)
	}

	fixture := openOrdinaryResumeSession(t, root, 0x71, 4)
	pauseOrdinaryResumeFixture(t, fixture)
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(*NativeResumeLease)
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := lease.store.Snapshot()
	if len(records) != 1 {
		t.Fatalf("resume records = %d", len(records))
	}
	item, err := blockedPublicationItem(records[0])
	if err != nil || item.State() != resumeauthority.ItemBlocked ||
		item.BlockReason() != resumeauthority.ItemBlockPublicationUnknown {
		t.Fatalf("publication mismatch item = (%+v, %v)", item, err)
	}
	item, err = blockedOwnedItem(records[0], fileexecution.OwnedObservation{})
	if err != nil || item.BlockReason() != resumeauthority.ItemBlockOwnedObjectUnknown {
		t.Fatalf("owned item = (%+v, %v)", item, err)
	}
	if disposition, err := ordinaryResumeRootDisposition(fixture.intent); err != nil ||
		disposition != outputcap.CallerProvidedContainer {
		t.Fatalf("single-file disposition = (%q, %v)", disposition, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinaryResumeItemReducesEveryDurableFilePhase(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x78, 4)
	if err := fixture.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(*NativeResumeLease)
	defer lease.Close()
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := lease.store.Snapshot()
	if len(records) != 1 {
		t.Fatalf("resume records = %d", len(records))
	}
	base := records[0]
	tests := []struct {
		name       string
		phase      checkpointmodel.Phase
		commit     checkpointmodel.CommitState
		ranges     []checkpointmodel.Range
		quarantine checkpointmodel.QuarantineReason
		origin     checkpointmodel.QuarantineOrigin
		retirement checkpointmodel.RetirementReason
		want       resumeauthority.ItemState
		block      resumeauthority.ItemBlockReason
	}{
		{
			name: "publishing with durable object", phase: checkpointmodel.PhasePublishing,
			commit: checkpointmodel.CommitVerified, ranges: base.VerifiedRanges(),
			want: resumeauthority.ItemResumable,
		},
		{
			name: "published without exact final", phase: checkpointmodel.PhasePublished,
			commit: checkpointmodel.CommitPublished, ranges: base.VerifiedRanges(),
			want: resumeauthority.ItemBlocked, block: resumeauthority.ItemBlockPublicationUnknown,
		},
		{
			name: "retired isolated failure", phase: checkpointmodel.PhaseRetired,
			commit: checkpointmodel.CommitVerified, ranges: base.VerifiedRanges(),
			retirement: checkpointmodel.RetirementIsolatedFailure,
			want:       resumeauthority.ItemFailed,
		},
		{
			name: "quarantined checkpoint", phase: checkpointmodel.PhaseQuarantined,
			commit: checkpointmodel.CommitQuarantined, ranges: base.VerifiedRanges(),
			quarantine: checkpointmodel.QuarantineStageMismatch,
			origin:     checkpointmodel.QuarantineOriginWitnessed,
			want:       resumeauthority.ItemBlocked, block: resumeauthority.ItemBlockCheckpointInvalid,
		},
		{
			name: "fresh active object", phase: checkpointmodel.PhaseActive,
			commit: checkpointmodel.CommitVerified,
			want:   resumeauthority.ItemIncomplete,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
				OperationID:                  base.OperationID(),
				ReceiveIntentDigest:          base.ReceiveIntentDigest(),
				MaterializationBindingDigest: base.MaterializationBindingDigest(),
				FileID:                       base.FileID(),
				FileRevision:                 base.FileRevision(),
				CanonicalPath:                base.CanonicalPath(),
				ExactSize:                    base.ExactSize(),
				MaterializerKind:             base.MaterializerKind(),
				AuthorityRef:                 base.AuthorityRef().Bytes(),
				OwnedObjectID:                base.OwnedObjectID().Bytes(),
				StateGeneration:              base.StateGeneration() + uint64(index) + 1,
				CheckpointGeneration:         base.CheckpointGeneration(),
				VerifiedRanges:               test.ranges,
				Phase:                        test.phase,
				CommitState:                  test.commit,
				QuarantineReason:             test.quarantine,
				QuarantineOrigin:             test.origin,
				RetirementReason:             test.retirement,
			})
			if err != nil {
				t.Fatal(err)
			}
			item, err := ordinaryResumeRecordItem(
				context.Background(),
				lease.topLevel,
				lease.store,
				record,
			)
			wantBlock := test.block
			if wantBlock == 0 {
				wantBlock = resumeauthority.ItemBlockNone
			}
			if err != nil || item.State() != test.want || item.BlockReason() != wantBlock {
				t.Fatalf("resume item = (%s, %s, %v), want (%s, %s)",
					item.State(), item.BlockReason(), err, test.want, wantBlock)
			}
		})
	}
}

func bytesToHex(raw []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(raw)*2)
	for index, value := range raw {
		encoded[index*2] = digits[value>>4]
		encoded[index*2+1] = digits[value&0x0f]
	}
	return string(encoded)
}
