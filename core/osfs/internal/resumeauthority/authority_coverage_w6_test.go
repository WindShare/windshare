package resumeauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type authorityMemoryStore struct {
	headers []Header
	leases  map[receivecontract.OperationID]*authorityMemoryLease
	busy    map[receivecontract.OperationID]bool
	pageErr error
}

func (store *authorityMemoryStore) Page(
	_ context.Context,
	cursor PageCursor,
	maximum int,
) (Page, error) {
	if store.pageErr != nil {
		return Page{}, store.pageErr
	}
	start := 0
	if !cursor.IsZero() {
		for index, header := range store.headers {
			if header.Record().OperationID() == cursor.After() {
				start = index + 1
				break
			}
		}
	}
	end := min(start+maximum, len(store.headers))
	var next PageCursor
	if end < len(store.headers) {
		next = NewPageCursor(store.headers[end-1].Record().OperationID())
	}
	return NewPage(store.headers[start:end], next, false)
}

func (store *authorityMemoryStore) Acquire(
	_ context.Context,
	operation receivecontract.OperationID,
) (OperationLease, error) {
	if store.busy[operation] {
		return nil, ErrBusy
	}
	lease := store.leases[operation]
	if lease == nil {
		return nil, ErrInvalidContract
	}
	return lease, nil
}

type authorityMemoryLease struct {
	header      Header
	items       []Item
	cleanup     CleanupState
	snapshotErr error
	cleanupErr  error
	closeErr    error
	transitions []checkpointmodel.OrdinaryLifecycleEvent
	closed      int
}

func (lease *authorityMemoryLease) Snapshot(context.Context) (Snapshot, error) {
	if lease.snapshotErr != nil {
		return Snapshot{}, lease.snapshotErr
	}
	return NewSnapshot(lease.header, lease.items)
}

func (lease *authorityMemoryLease) Transition(
	_ context.Context,
	event checkpointmodel.OrdinaryLifecycleEvent,
	reason checkpointmodel.OrdinaryClosedReason,
) (Header, error) {
	previous := lease.header.Record()
	state, closedReason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), event, reason,
	)
	if err != nil {
		return Header{}, err
	}
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: state, Lease: previous.Lease(), ClosedReason: closedReason,
		},
	)
	if err != nil {
		return Header{}, err
	}
	header, err := NewHeader(next)
	if err == nil {
		lease.header = header
		lease.transitions = append(lease.transitions, event)
	}
	return header, err
}

func (lease *authorityMemoryLease) Cleanup(context.Context) (CleanupState, error) {
	return lease.cleanup, lease.cleanupErr
}

func (lease *authorityMemoryLease) Close() error {
	lease.closed++
	return lease.closeErr
}

func heldResumeRecord(
	t *testing.T,
	record checkpointmodel.OrdinaryOperationRecord,
) checkpointmodel.OrdinaryOperationRecord {
	t.Helper()
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		record,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: record.Lifecycle(), Lease: checkpointmodel.OrdinaryLeaseHeld,
			ClosedReason: record.ClosedReason(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestAuthorityPagesAndEnumeratesOnlyAcquiredOperations(t *testing.T) {
	firstFixture := newOrdinaryResumeFixture(t, 0x81)
	secondFixture := newOrdinaryResumeFixture(t, 0x91)
	firstHeader := resumeHeader(t, heldResumeRecord(t, firstFixture.record))
	secondHeader := resumeHeader(t, heldResumeRecord(t, secondFixture.record))
	partial := resumeItem(t, "result/partial", ItemResumable)
	secondLease := &authorityMemoryLease{
		header: secondHeader, items: []Item{partial}, cleanup: CleanupComplete,
	}
	store := &authorityMemoryStore{
		headers: []Header{firstHeader, secondHeader},
		leases: map[receivecontract.OperationID]*authorityMemoryLease{
			secondHeader.Record().OperationID(): secondLease,
		},
		busy: map[receivecontract.OperationID]bool{
			firstHeader.Record().OperationID(): true,
		},
	}
	authority, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.List(context.Background())
	if err != nil || len(inventory.Summaries()) != 2 {
		t.Fatalf("inventory = %+v, %v", inventory, err)
	}
	summaries := inventory.Summaries()
	if !summaries[0].Busy() || summaries[0].State() != OperationIncomplete ||
		summaries[1].State() != OperationResumable || secondLease.closed != 1 {
		t.Fatalf("summaries = %+v, closed=%d", summaries, secondLease.closed)
	}
}

func TestAuthorityDiscardTransitionsBeforeCleanupAndDeletesHistory(t *testing.T) {
	fixture := newOrdinaryResumeFixture(t, 0xa1)
	header := resumeHeader(t, heldResumeRecord(t, fixture.record))
	lease := &authorityMemoryLease{header: header, cleanup: CleanupComplete}
	store := &authorityMemoryStore{
		leases: map[receivecontract.OperationID]*authorityMemoryLease{
			header.Record().OperationID(): lease,
		},
		busy: make(map[receivecontract.OperationID]bool),
	}
	authority, _ := New(store)
	summary, err := authority.Discard(context.Background(), header.Record().OperationID())
	if err != nil || summary.State() != OperationDiscarded || lease.closed != 1 ||
		len(lease.transitions) != 1 ||
		lease.transitions[0] != checkpointmodel.OrdinaryLifecycleDiscard {
		t.Fatalf("discard = %+v, %v, transitions=%v", summary, err, lease.transitions)
	}
}

func TestAuthorityPersistsCleanupPendingWithoutEscalatingItems(t *testing.T) {
	fixture := newOrdinaryResumeFixture(t, 0xb1)
	attentionRecord := resumeRecordState(
		t, fixture.record, checkpointmodel.OrdinaryOperationNeedsAttention,
		checkpointmodel.OrdinaryReasonOperationOwnershipUnknown,
	)
	header := resumeHeader(t, heldResumeRecord(t, attentionRecord))
	blocked, _ := NewItem("result/blocked", ItemBlocked, ItemBlockCheckpointInvalid)
	lease := &authorityMemoryLease{
		header: header, items: []Item{blocked}, cleanup: CleanupPending,
	}
	store := &authorityMemoryStore{
		leases: map[receivecontract.OperationID]*authorityMemoryLease{
			header.Record().OperationID(): lease,
		},
		busy: make(map[receivecontract.OperationID]bool),
	}
	authority, _ := New(store)
	summary, err := authority.Discard(context.Background(), header.Record().OperationID())
	if err != nil || summary.State() != OperationCleanupPending ||
		len(summary.Items()) != 1 || len(lease.transitions) != 2 ||
		lease.transitions[0] != checkpointmodel.OrdinaryLifecycleDiscard ||
		lease.transitions[1] != checkpointmodel.OrdinaryLifecycleCleanupFailed {
		t.Fatalf("pending discard = %+v, %v, transitions=%v", summary, err, lease.transitions)
	}
}

func TestAuthorityRejectsInvalidBoundariesAndJoinsCloseFailure(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil store = %v", err)
	}
	fixture := newOrdinaryResumeFixture(t, 0xc1)
	header := resumeHeader(t, heldResumeRecord(t, fixture.record))
	closeErr := errors.New("close")
	lease := &authorityMemoryLease{
		header: header, cleanup: CleanupComplete, closeErr: closeErr,
	}
	store := &authorityMemoryStore{
		headers: []Header{header},
		leases: map[receivecontract.OperationID]*authorityMemoryLease{
			header.Record().OperationID(): lease,
		},
		busy: make(map[receivecontract.OperationID]bool),
	}
	authority, _ := New(store)
	if _, err := authority.List(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("list close error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.List(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list = %v", err)
	}
	if _, err := authority.Discard(context.Background(), receivecontract.OperationID{}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("zero discard = %v", err)
	}
}
