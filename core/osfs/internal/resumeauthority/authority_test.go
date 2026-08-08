package resumeauthority

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestNativeAuthorityDiscardsOwnedWitnessesInDurableOrder(t *testing.T) {
	binding, record := resumeRecord(
		t, 0x51, 0x71, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished,
	)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)
	fixture := newAuthorityExecutorFixture(t, snapshot, map[checkpointmodel.RecordID]Evidence{
		record.RecordID(): EvidenceExact,
	})

	result, err := Discard(context.Background(), fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != Discarded || result.RemovedArtifacts() != 3 {
		t.Fatalf("discard result = %v/%d", result.Status(), result.RemovedArtifacts())
	}
	want := []ActionKind{
		ActionRemoveStage, ActionSyncStages,
		ActionRemoveAnchor, ActionSyncAnchors,
		ActionRemoveRecord, ActionSyncRecords,
	}
	if !reflect.DeepEqual(fixture.leased.actionKinds(), want) {
		t.Fatalf("actions = %v, want %v", fixture.leased.actionKinds(), want)
	}
	if fixture.observer.pins[0].revalidationCalls != len(want)*2 {
		t.Fatalf("publication revalidations = %d", fixture.observer.pins[0].revalidationCalls)
	}
	if fixture.leased.closeCalls != 1 || fixture.observer.pins[0].closeCalls != 1 {
		t.Fatal("discard did not close the leased repository and publication pin")
	}
	if err := fixture.inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if *fixture.platformCloseCalls != 1 {
		t.Fatalf("platform close calls = %d", *fixture.platformCloseCalls)
	}
}

func TestNativeAuthorityStopsWhenPublishedIdentityChangesAcrossAnAction(t *testing.T) {
	binding, record := resumeRecord(
		t, 0x52, 0x72, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished,
	)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)
	fixture := newAuthorityExecutorFixture(t, snapshot, map[checkpointmodel.RecordID]Evidence{
		record.RecordID(): EvidenceExact,
	})
	fixture.observer.revalidate[record.RecordID()] = []Evidence{EvidenceExact, EvidenceReplaced}

	result, err := Discard(context.Background(), fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != DiscardNeedsAttention || result.RemovedArtifacts() != 1 ||
		len(result.Attention()) != 1 || result.Attention()[0].Reason() != AttentionReplacement {
		t.Fatalf("discard result = %+v", result)
	}
	if got := fixture.leased.actionKinds(); !reflect.DeepEqual(got, []ActionKind{ActionRemoveStage}) {
		t.Fatalf("actions after replacement = %v", got)
	}
	if err := fixture.inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAuthoritySettlesAlreadyAbsentAndUnsafeStateWithoutMutation(t *testing.T) {
	binding, record := resumeRecord(
		t, 0x53, 0x73, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified,
	)
	tests := []struct {
		name       string
		snapshot   RepositorySnapshot
		want       DiscardStatus
		observer   map[checkpointmodel.RecordID]Evidence
		wantReason AttentionReason
	}{
		{
			name:     "already absent",
			snapshot: mustSnapshot(t, binding, nil, nil),
			want:     AlreadyAbsent,
			observer: map[checkpointmodel.RecordID]Evidence{},
		},
		{
			name: "repository attention",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact),
			}, []Attention{derivedAttention(AttentionUnknownChildren, []byte("opaque"))}),
			want:       DiscardNeedsAttention,
			observer:   map[checkpointmodel.RecordID]Evidence{record.RecordID(): EvidenceAbsent},
			wantReason: AttentionUnknownChildren,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthorityExecutorFixture(t, test.snapshot, test.observer)
			result, err := Discard(context.Background(), fixture.reference)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status() != test.want || len(fixture.leased.actionKinds()) != 0 ||
				len(fixture.observer.pins) != 0 {
				t.Fatalf("settlement = %+v, actions = %v, publication pins = %d",
					result, fixture.leased.actionKinds(), len(fixture.observer.pins))
			}
			if test.wantReason.Valid() && !hasAttentionReason(result.Attention(), test.wantReason) {
				t.Fatalf("attention = %+v", result.Attention())
			}
			if err := fixture.inventory.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeAuthorityBusyConsumesReferenceWithoutMutation(t *testing.T) {
	binding, record := resumeRecord(
		t, 0x54, 0x74, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified,
	)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)
	fixture := newAuthorityExecutorFixture(t, snapshot, map[checkpointmodel.RecordID]Evidence{
		record.RecordID(): EvidenceAbsent,
	})
	fixture.pinned.acquireErr = NewRepositoryError(RepositoryBusy, "acquire", errors.New("held"))

	if _, err := Discard(context.Background(), fixture.reference); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy discard error = %v", err)
	}
	if _, err := Discard(context.Background(), fixture.reference); !errors.Is(err, ErrReferenceConsumed) {
		t.Fatalf("reused busy reference error = %v", err)
	}
	if len(fixture.leased.actionKinds()) != 0 || len(fixture.observer.pins) != 0 {
		t.Fatal("busy discard obtained mutation authority")
	}
	if err := fixture.inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRepositoryOwnsCloserOnFailedListAndRejectsInvalidLease(t *testing.T) {
	closeCalls := 0
	listFailure := errors.New("repository list failed")
	repository, err := NewNativeRepository(
		&authorityTestStore{err: listFailure},
		&authorityPublicationObserver{},
		func() error { closeCalls++; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListResumeState(context.Background()); !errors.Is(err, listFailure) {
		t.Fatalf("failed list error = %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("closer calls = %d", closeCalls)
	}

	pinned := &authorityExecutorInventory{entries: []ListedState{
		listedState(t, 0x55, ListAvailable, nil),
	}}
	store := &authorityTestStore{pinned: pinned}
	repository, err = NewNativeRepository(store, &authorityPublicationObserver{}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := repository.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.Acquire(context.Background(), 0); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil leased repository error = %v", err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

type authorityExecutorFixture struct {
	inventory          *Inventory
	reference          Reference
	pinned             *authorityExecutorInventory
	leased             *authorityExecutorLease
	observer           *authorityPublicationObserver
	platformCloseCalls *int
}

func newAuthorityExecutorFixture(
	t *testing.T,
	snapshot RepositorySnapshot,
	publication map[checkpointmodel.RecordID]Evidence,
) authorityExecutorFixture {
	t.Helper()
	leased := &authorityExecutorLease{snapshot: snapshot, checkpoints: make(map[checkpointmodel.RecordID]checkpointmodel.Record)}
	for _, checkpoint := range snapshot.Checkpoints() {
		leased.checkpoints[checkpoint.RecordID()] = checkpoint.Record()
	}
	pinned := &authorityExecutorInventory{
		entries: []ListedState{listedState(t, 0x56, ListAvailable, nil)}, leased: leased,
	}
	observer := &authorityPublicationObserver{
		initial: publication, revalidate: make(map[checkpointmodel.RecordID][]Evidence),
	}
	platformCloseCalls := 0
	native, err := NewNativeRepository(
		&authorityTestStore{pinned: pinned}, observer,
		func() error { platformCloseCalls++; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := List(context.Background(), native)
	if err != nil {
		t.Fatal(err)
	}
	return authorityExecutorFixture{
		inventory: inventory, reference: inventory.Summaries()[0].Reference(),
		pinned: pinned, leased: leased, observer: observer,
		platformCloseCalls: &platformCloseCalls,
	}
}

type authorityTestStore struct {
	pinned PinnedInventory
	err    error
}

func (store *authorityTestStore) ListResumeState(context.Context) (PinnedInventory, error) {
	return store.pinned, store.err
}

type authorityExecutorInventory struct {
	entries    []ListedState
	leased     LeasedRepository
	acquireErr error
	closeCalls int
}

func (inventory *authorityExecutorInventory) Entries() []ListedState {
	return append([]ListedState(nil), inventory.entries...)
}

func (inventory *authorityExecutorInventory) Acquire(context.Context, int) (LeasedRepository, error) {
	if inventory.acquireErr != nil {
		return nil, inventory.acquireErr
	}
	return inventory.leased, nil
}

func (inventory *authorityExecutorInventory) Close() error {
	inventory.closeCalls++
	return nil
}

type authorityExecutorLease struct {
	snapshot    RepositorySnapshot
	checkpoints map[checkpointmodel.RecordID]checkpointmodel.Record
	actions     []Action
	closeCalls  int
}

func (lease *authorityExecutorLease) Observe(context.Context) (RepositorySnapshot, error) {
	return lease.snapshot, nil
}

func (lease *authorityExecutorLease) Apply(_ context.Context, action Action) (ApplyResult, error) {
	lease.actions = append(lease.actions, action)
	return NewApplyResult(ApplyCompleted, nil)
}

func (lease *authorityExecutorLease) Close() error {
	lease.closeCalls++
	return nil
}

func (lease *authorityExecutorLease) PinnedCheckpoint(
	recordID checkpointmodel.RecordID,
) (PinnedCheckpoint, bool) {
	record, ok := lease.checkpoints[recordID]
	if !ok {
		return nil, false
	}
	return authorityPinnedCheckpoint{record: record}, true
}

func (lease *authorityExecutorLease) actionKinds() []ActionKind {
	result := make([]ActionKind, len(lease.actions))
	for index, action := range lease.actions {
		result[index] = action.Kind()
	}
	return result
}

type authorityPinnedCheckpoint struct {
	record checkpointmodel.Record
}

func (checkpoint authorityPinnedCheckpoint) Record() checkpointmodel.Record { return checkpoint.record }

func (authorityPinnedCheckpoint) SameOwnedFile(
	context.Context,
	outputcap.File,
) (Evidence, error) {
	return EvidenceExact, nil
}

type authorityPublicationObserver struct {
	initial    map[checkpointmodel.RecordID]Evidence
	revalidate map[checkpointmodel.RecordID][]Evidence
	pins       []*authorityPublicationPin
}

func (observer *authorityPublicationObserver) PinPublication(
	_ context.Context,
	checkpoint PinnedCheckpoint,
) (PinnedPublication, error) {
	recordID := checkpoint.Record().RecordID()
	observation, err := NewPublicationObservation(recordID, observer.initial[recordID])
	if err != nil {
		return nil, err
	}
	pin := &authorityPublicationPin{
		observation: observation,
		revalidate:  append([]Evidence(nil), observer.revalidate[recordID]...),
	}
	observer.pins = append(observer.pins, pin)
	return pin, nil
}

type authorityPublicationPin struct {
	observation       PublicationObservation
	revalidate        []Evidence
	revalidationCalls int
	closeCalls        int
}

func (pin *authorityPublicationPin) Observation() PublicationObservation { return pin.observation }

func (pin *authorityPublicationPin) Revalidate(context.Context) (Evidence, error) {
	index := pin.revalidationCalls
	pin.revalidationCalls++
	if index < len(pin.revalidate) {
		return pin.revalidate[index], nil
	}
	return pin.observation.FinalEvidence(), nil
}

func (pin *authorityPublicationPin) Close() error {
	pin.closeCalls++
	return nil
}

var _ Repository = (*authorityTestStore)(nil)
var _ PinnedInventory = (*authorityExecutorInventory)(nil)
var _ PinnedCheckpointProvider = (*authorityExecutorLease)(nil)
var _ PublicationObserver = (*authorityPublicationObserver)(nil)
