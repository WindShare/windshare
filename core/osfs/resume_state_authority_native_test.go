package osfs

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type publicOrdinaryFixture struct {
	header    resumeauthority.Header
	operation receivecontract.OperationID
	intent    transfer.ReceiveIntentDigest
}

func newPublicOrdinaryFixture(t *testing.T, seed byte) publicOrdinaryFixture {
	t.Helper()
	var share catalog.ShareInstance
	var root catalog.DirectoryID
	share[0], root[0] = seed, seed+1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, _ := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{seed + 2}, receivecontract.StableIdentityBytes),
	)
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(
		bytes.Repeat([]byte{seed + 3}, receivecontract.StableIdentityBytes),
	)
	authorityRef, _ := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{seed + 4}, receivecontract.AuthorityRefBytes),
	)
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authorityRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	activeKey, err := checkpointmodel.NewActiveOperationKeyV1(selection.Digest(), authorityRef)
	if err != nil {
		t.Fatal(err)
	}
	var token [32]byte
	token[0] = seed + 5
	claim, err := checkpointmodel.NewReservationClaimLocator(token, 1)
	if err != nil {
		t.Fatal(err)
	}
	record, err := checkpointmodel.NewOrdinaryOperationRecord(
		checkpointmodel.OrdinaryOperationRecordSpec{
			ActiveKey:           activeKey,
			Intent:              intent,
			ReservationClaim:    claim,
			LifecycleGeneration: 1,
			Lifecycle:           checkpointmodel.OrdinaryOperationActive,
			Lease:               checkpointmodel.OrdinaryLeaseReleased,
			ClosedReason:        checkpointmodel.OrdinaryReasonNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	header, err := resumeauthority.NewHeader(record)
	if err != nil {
		t.Fatal(err)
	}
	return publicOrdinaryFixture{
		header: header, operation: operation, intent: intent.Digest(),
	}
}

type publicOrdinaryStore struct {
	header     resumeauthority.Header
	lease      *publicOrdinaryLease
	unknown    bool
	busy       bool
	pageErr    error
	acquireErr error
	acquires   int
}

func (store *publicOrdinaryStore) Page(
	_ context.Context,
	cursor resumeauthority.PageCursor,
	maximum int,
) (resumeauthority.Page, error) {
	if store.pageErr != nil {
		return resumeauthority.Page{}, store.pageErr
	}
	if maximum <= 0 || !cursor.IsZero() || !store.header.Valid() {
		return resumeauthority.NewPage(nil, resumeauthority.PageCursor{}, store.unknown)
	}
	return resumeauthority.NewPage(
		[]resumeauthority.Header{store.header},
		resumeauthority.PageCursor{},
		store.unknown,
	)
}

func (store *publicOrdinaryStore) Acquire(
	_ context.Context,
	operation receivecontract.OperationID,
) (resumeauthority.OperationLease, error) {
	store.acquires++
	if store.acquireErr != nil {
		return nil, store.acquireErr
	}
	if store.busy {
		return nil, resumeauthority.ErrBusy
	}
	if store.lease == nil || operation != store.header.Record().OperationID() {
		return nil, resumeauthority.ErrInvalidContract
	}
	return store.lease, nil
}

type publicOrdinaryLease struct {
	header        resumeauthority.Header
	items         []resumeauthority.Item
	cleanup       resumeauthority.CleanupState
	snapshotErr   error
	transitionErr error
	cleanupErr    error
	closeErr      error
	snapshots     int
	transitions   []checkpointmodel.OrdinaryLifecycleEvent
	closed        int
}

func (lease *publicOrdinaryLease) Snapshot(
	context.Context,
) (resumeauthority.Snapshot, error) {
	lease.snapshots++
	if lease.snapshotErr != nil {
		return resumeauthority.Snapshot{}, lease.snapshotErr
	}
	return resumeauthority.NewSnapshot(lease.header, lease.items)
}

func (lease *publicOrdinaryLease) Transition(
	_ context.Context,
	event checkpointmodel.OrdinaryLifecycleEvent,
	reason checkpointmodel.OrdinaryClosedReason,
) (resumeauthority.Header, error) {
	if lease.transitionErr != nil {
		return resumeauthority.Header{}, lease.transitionErr
	}
	previous := lease.header.Record()
	lifecycle, closedReason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), event, reason,
	)
	if err != nil {
		return resumeauthority.Header{}, err
	}
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: lifecycle, Lease: previous.Lease(), ClosedReason: closedReason,
		},
	)
	if err != nil {
		return resumeauthority.Header{}, err
	}
	header, err := resumeauthority.NewHeader(next)
	if err == nil {
		lease.header = header
		lease.transitions = append(lease.transitions, event)
	}
	return header, err
}

func (lease *publicOrdinaryLease) Cleanup(context.Context) (resumeauthority.CleanupState, error) {
	return lease.cleanup, lease.cleanupErr
}

func (lease *publicOrdinaryLease) Close() error {
	lease.closed++
	return lease.closeErr
}

func publicResumeItem(
	t *testing.T,
	path string,
	state resumeauthority.ItemState,
	reason resumeauthority.ItemBlockReason,
) resumeauthority.Item {
	t.Helper()
	item, err := resumeauthority.NewItem(path, state, reason)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestRepositoryResumeStateAuthorityProjectsOrdinaryInventory(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x31)
	resumable := publicResumeItem(
		t, "result/partial", resumeauthority.ItemResumable, resumeauthority.ItemBlockNone,
	)
	blocked := publicResumeItem(
		t, "result/blocked", resumeauthority.ItemBlocked,
		resumeauthority.ItemBlockPublicationUnknown,
	)
	lease := &publicOrdinaryLease{
		header: fixture.header, items: []resumeauthority.Item{resumable, blocked},
		cleanup: resumeauthority.CleanupComplete,
	}
	store := &publicOrdinaryStore{header: fixture.header, lease: lease}
	authority, err := newResumeStateAuthority(store)
	if err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.ListResumeState(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != ResumeStateListReady ||
		inventory.UnknownEntries() || len(summaries) != 1 {
		t.Fatalf("inventory = (%+v, %v)", inventory, err)
	}
	summary := summaries[0]
	items := summary.Items()
	if !summary.Valid() || !summary.Resumable() ||
		summary.OperationID() != fixture.operation ||
		summary.ReceiveIntentDigest() != fixture.intent ||
		summary.StateGeneration() != 1 ||
		summary.State() != ResumeOperationResumable ||
		len(items) != 2 ||
		items[0].CanonicalPath() != "result/blocked" ||
		items[0].State() != ResumeItemBlocked ||
		items[0].BlockReason() != ResumeItemBlockPublicationUnknown ||
		items[1].State() != ResumeItemResumable {
		t.Fatalf("summary = %+v, items=%+v", summary, items)
	}

	items[0] = ResumeStateItem{}
	summaries[0] = ResumeStateSummary{}
	if summary.Items()[0].CanonicalPath() != "result/blocked" ||
		inventory.Summaries()[0].OperationID() != fixture.operation {
		t.Fatal("public projections exposed mutable backing storage")
	}
	if ResumeOperationResumable.String() != "resumable" ||
		ResumeItemBlocked.String() != "item-blocked" ||
		ResumeItemBlockPublicationUnknown.String() != "publication-unknown" ||
		ResumeOperationState(0).String() != "" ||
		ResumeItemState(0).String() != "" ||
		ResumeItemBlockReason(0).String() != "" {
		t.Fatal("resume vocabulary is not stable")
	}
}

func TestRepositoryResumeStateAuthoritySurfacesBusyAndUnknownInventory(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x41)
	store := &publicOrdinaryStore{
		header: fixture.header, unknown: true, busy: true,
	}
	authority, err := newResumeStateAuthority(store)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != ResumeStateListNeedsAttention ||
		!inventory.UnknownEntries() || len(summaries) != 1 ||
		!summaries[0].Busy() || summaries[0].State() != ResumeOperationIncomplete {
		t.Fatalf("busy inventory = (%+v, %v)", inventory, err)
	}
	if store.acquires != 1 {
		t.Fatalf("operation acquisitions = %d", store.acquires)
	}
}

func TestRepositoryResumeStateAuthorityDeindexesBeforeSafeDiscard(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x51)
	lease := &publicOrdinaryLease{
		header: fixture.header, cleanup: resumeauthority.CleanupComplete,
	}
	authority, err := newResumeStateAuthority(&publicOrdinaryStore{
		header: fixture.header, lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := authority.Discard(context.Background(), fixture.operation)
	if err != nil || summary.State() != ResumeOperationDiscarded ||
		!slices.Equal(lease.transitions, []checkpointmodel.OrdinaryLifecycleEvent{
			checkpointmodel.OrdinaryLifecycleDiscard,
		}) || lease.closed != 1 {
		t.Fatalf("discard = (%+v, %v), transitions=%v", summary, err, lease.transitions)
	}

	pendingFixture := newPublicOrdinaryFixture(t, 0x61)
	pendingLease := &publicOrdinaryLease{
		header: pendingFixture.header, cleanup: resumeauthority.CleanupPending,
	}
	pendingAuthority, _ := newResumeStateAuthority(&publicOrdinaryStore{
		header: pendingFixture.header, lease: pendingLease,
	})
	pending, err := pendingAuthority.Discard(context.Background(), pendingFixture.operation)
	if err != nil || pending.State() != ResumeOperationCleanupPending ||
		pending.NeedsAttentionReason() != checkpointmodel.OrdinaryReasonCleanupUncertain.String() ||
		!slices.Equal(pendingLease.transitions, []checkpointmodel.OrdinaryLifecycleEvent{
			checkpointmodel.OrdinaryLifecycleDiscard,
			checkpointmodel.OrdinaryLifecycleCleanupFailed,
		}) {
		t.Fatalf("pending discard = (%+v, %v), transitions=%v", pending, err, pendingLease.transitions)
	}
}

func TestRepositoryResumeStateAuthorityRejectsInvalidPublicBoundaries(t *testing.T) {
	if _, err := newResumeStateAuthority(nil); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil store error = %v", err)
	}
	var authority *RepositoryResumeStateAuthority
	if _, err := authority.ListResumeState(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil list error = %v", err)
	}
	if _, err := authority.Discard(
		context.Background(), receivecontract.OperationID{},
	); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil discard error = %v", err)
	}
}
