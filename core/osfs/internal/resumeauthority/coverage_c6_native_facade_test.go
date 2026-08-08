package resumeauthority

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

func TestCoverageC6NativeRepositoryConsumesConstructionAuthorityExactlyOnce(t *testing.T) {
	observer := coverageC6PublicationObserverFunc(func(context.Context, PinnedCheckpoint) (PinnedPublication, error) {
		return nil, errors.New("unused observer")
	})
	store := &coverageC6Store{}
	closer := func() error { return nil }
	for _, test := range []struct {
		name     string
		store    Repository
		observer PublicationObserver
		closer   func() error
	}{
		{name: "store", observer: observer, closer: closer},
		{name: "observer", store: store, closer: closer},
		{name: "closer", store: store, observer: observer},
	} {
		t.Run(test.name, func(t *testing.T) {
			if repository, err := NewNativeRepository(test.store, test.observer, test.closer); repository != nil ||
				!errors.Is(err, ErrInvalidContract) {
				t.Fatalf("invalid constructor result = (%v, %v)", repository, err)
			}
		})
	}

	var nilRepository *NativeRepository
	if inventory, err := nilRepository.ListResumeState(context.Background()); inventory != nil ||
		!errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil repository list = (%v, %v)", inventory, err)
	}

	listFailure := errors.New("list failed")
	closeFailure := errors.New("platform close failed")
	closeCalls := 0
	repository, err := NewNativeRepository(
		&coverageC6Store{err: listFailure}, observer,
		func() error { closeCalls++; return closeFailure },
	)
	if err != nil {
		t.Fatal(err)
	}
	if inventory, err := repository.ListResumeState(context.Background()); inventory != nil ||
		!errors.Is(err, listFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("failed list result = (%v, %v)", inventory, err)
	}
	if _, err := repository.ListResumeState(context.Background()); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("second list error = %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("failed list close calls = %d", closeCalls)
	}

	for _, test := range []struct {
		name   string
		ctx    context.Context
		pinned PinnedInventory
		want   error
	}{
		{name: "nil context", pinned: &authorityExecutorInventory{}, want: ErrInvalidContract},
		{name: "canceled context", ctx: coverageC6CanceledContext(), pinned: &authorityExecutorInventory{}, want: context.Canceled},
		{name: "nil inventory", ctx: context.Background(), want: ErrInvalidContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			native, err := NewNativeRepository(
				&coverageC6Store{pinned: test.pinned}, observer,
				func() error { calls++; return nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if inventory, err := native.ListResumeState(test.ctx); inventory != nil || !errors.Is(err, test.want) {
				t.Fatalf("list result = (%v, %v), want %v", inventory, err, test.want)
			}
			if calls != 1 {
				t.Fatalf("closer calls = %d", calls)
			}
		})
	}
}

func TestCoverageC6NativeInventoryClosesPinsBeforePlatformAndCachesJoinedFailure(t *testing.T) {
	pinnedFailure := errors.New("pinned inventory close failed")
	platformFailure := errors.New("platform close failed")
	var order []string
	base := &authorityExecutorInventory{entries: []ListedState{
		listedState(t, 0xc1, ListAvailable, nil),
	}}
	pinned := &coverageC6ClosingInventory{
		PinnedInventory: base,
		close: func() error {
			order = append(order, "pinned")
			return pinnedFailure
		},
	}
	native, err := NewNativeRepository(
		&coverageC6Store{pinned: pinned},
		coverageC6PublicationObserverFunc(func(context.Context, PinnedCheckpoint) (PinnedPublication, error) {
			return nil, ErrInvalidContract
		}),
		func() error {
			order = append(order, "platform")
			return platformFailure
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := native.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if entries := inventory.Entries(); len(entries) != 1 {
		t.Fatalf("live entries = %+v", entries)
	}
	for attempt := range 2 {
		if err := inventory.Close(); !errors.Is(err, pinnedFailure) || !errors.Is(err, platformFailure) {
			t.Fatalf("close %d error = %v", attempt+1, err)
		}
	}
	if !reflect.DeepEqual(order, []string{"pinned", "platform"}) || pinned.closeCalls != 1 {
		t.Fatalf("close order/calls = %v/%d", order, pinned.closeCalls)
	}
	if entries := inventory.Entries(); entries != nil {
		t.Fatalf("closed inventory retained entries: %+v", entries)
	}
	if leased, err := inventory.Acquire(context.Background(), 0); leased != nil || !errors.Is(err, ErrInventoryClosed) {
		t.Fatalf("closed inventory acquire = (%v, %v)", leased, err)
	}

	var nilInventory *nativeInventory
	if nilInventory.Entries() != nil {
		t.Fatal("nil inventory returned entries")
	}
	if leased, err := nilInventory.Acquire(context.Background(), 0); leased != nil || !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil inventory acquire = (%v, %v)", leased, err)
	}
	if err := nilInventory.Close(); err != nil {
		t.Fatalf("nil inventory close = %v", err)
	}
}

func TestCoverageC6NativeInventoryRejectsLeaseWithoutCheckpointPinsAndClosesIt(t *testing.T) {
	closeFailure := errors.New("lease close failed")
	lease := &coverageC6BareLease{closeErr: closeFailure}
	pinned := &authorityExecutorInventory{
		entries: []ListedState{listedState(t, 0xc2, ListAvailable, nil)},
		leased:  lease,
	}
	native, err := NewNativeRepository(
		&coverageC6Store{pinned: pinned},
		coverageC6PublicationObserverFunc(func(context.Context, PinnedCheckpoint) (PinnedPublication, error) {
			return nil, ErrInvalidContract
		}),
		func() error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := native.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if leased, err := inventory.Acquire(context.Background(), 0); leased != nil ||
		!errors.Is(err, ErrInvalidContract) || !errors.Is(err, closeFailure) {
		t.Fatalf("invalid lease acquire = (%v, %v)", leased, err)
	}
	if lease.closeCalls != 1 {
		t.Fatalf("invalid lease close calls = %d", lease.closeCalls)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageC6NativePublicationPinFailuresReleaseEveryAcquiredPin(t *testing.T) {
	binding, firstRecord := resumeRecord(
		t, 0xc3, 0xd3, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished,
	)
	_, secondRecord := resumeRecord(
		t, 0xc4, 0xd4, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished,
	)
	firstSnapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, firstRecord, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)
	twoSnapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, firstRecord, EvidenceExact, EvidenceExact, EvidenceExact),
		mustCheckpointObservation(t, secondRecord, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)

	provider := &authorityExecutorLease{checkpoints: map[checkpointmodel.RecordID]checkpointmodel.Record{
		firstRecord.RecordID(): firstRecord,
	}}
	repository := &nativeLeasedRepository{
		LeasedRepository: provider, provider: provider,
		observer: coverageC6PublicationObserverFunc(func(context.Context, PinnedCheckpoint) (PinnedPublication, error) {
			return nil, ErrInvalidContract
		}),
	}
	provider.checkpoints = map[checkpointmodel.RecordID]checkpointmodel.Record{}
	if publications, err := repository.PinPublications(context.Background(), firstSnapshot); publications != nil ||
		!errors.Is(err, ErrInvalidContract) {
		t.Fatalf("missing checkpoint pins = (%v, %v)", publications, err)
	}
	provider.checkpoints[firstRecord.RecordID()] = secondRecord
	if publications, err := repository.PinPublications(context.Background(), firstSnapshot); publications != nil ||
		!errors.Is(err, ErrInvalidContract) {
		t.Fatalf("mismatched checkpoint pins = (%v, %v)", publications, err)
	}

	provider.checkpoints = map[checkpointmodel.RecordID]checkpointmodel.Record{
		firstRecord.RecordID():  firstRecord,
		secondRecord.RecordID(): secondRecord,
	}
	observerFailure := errors.New("second publication observation failed")
	firstPin := &coverageC6PublicationPin{
		observation: mustPublicationObservation(t, firstRecord.RecordID(), EvidenceExact),
	}
	calls := 0
	repository.observer = coverageC6PublicationObserverFunc(func(
		_ context.Context,
		checkpoint PinnedCheckpoint,
	) (PinnedPublication, error) {
		calls++
		if checkpoint.Record().RecordID() == firstRecord.RecordID() {
			return firstPin, nil
		}
		return nil, observerFailure
	})
	if publications, err := repository.PinPublications(context.Background(), twoSnapshot); publications != nil ||
		!errors.Is(err, observerFailure) {
		t.Fatalf("partial publication pins = (%v, %v)", publications, err)
	}
	if calls != 2 || firstPin.closeCalls != 1 {
		t.Fatalf("observer calls/first pin closes = %d/%d", calls, firstPin.closeCalls)
	}

	wrongPin := &coverageC6PublicationPin{
		observation: mustPublicationObservation(t, secondRecord.RecordID(), EvidenceExact),
	}
	repository.observer = coverageC6PublicationObserverFunc(func(context.Context, PinnedCheckpoint) (PinnedPublication, error) {
		return wrongPin, nil
	})
	if publications, err := repository.PinPublications(context.Background(), firstSnapshot); publications != nil ||
		!errors.Is(err, ErrInvalidContract) {
		t.Fatalf("wrong-record publication pin = (%v, %v)", publications, err)
	}
	if wrongPin.closeCalls != 1 {
		t.Fatalf("wrong-record pin close calls = %d", wrongPin.closeCalls)
	}
}

func TestCoverageC6AuthorityFacadeRejectsCancellationAndMissingPublicationContract(t *testing.T) {
	if inventory, err := List(context.Background(), nil); inventory != nil || !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil-repository list = (%v, %v)", inventory, err)
	}
	store := &coverageC6Store{}
	if inventory, err := List(coverageC6CanceledContext(), store); inventory != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list = (%v, %v)", inventory, err)
	}
	if store.calls != 0 {
		t.Fatalf("canceled list called repository %d times", store.calls)
	}
	listFailure := errors.New("repository list failed")
	if inventory, err := List(context.Background(), &coverageC6Store{err: listFailure}); inventory != nil ||
		!errors.Is(err, listFailure) {
		t.Fatalf("failed list = (%v, %v)", inventory, err)
	}
	binding, record := resumeRecord(
		t, 0xc5, 0xd5, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified,
	)
	lease := &coverageC6BareLease{snapshot: mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)}
	pinned := &authorityExecutorInventory{
		entries: []ListedState{listedState(t, 0xc6, ListAvailable, nil)}, leased: lease,
	}
	inventory, err := NewInventory(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Discard(context.Background(), inventory.Summaries()[0].Reference()); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("missing publication contract error = %v", err)
	}
	if lease.closeCalls != 1 {
		t.Fatalf("missing-contract lease close calls = %d", lease.closeCalls)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageC6RepositoryErrorRetainsOnlyClosedPolicyFields(t *testing.T) {
	if err := NewRepositoryError(RepositoryStateIO, "read", nil); err != nil {
		t.Fatalf("nil cause projected as %v", err)
	}
	cause := errors.New("diagnostic cause")
	err := NewRepositoryError(RepositoryStateIO, "read", cause)
	var projected *RepositoryError
	if !errors.As(err, &projected) {
		t.Fatalf("repository error type = %T", err)
	}
	if projected.Code() != RepositoryStateIO || projected.Operation() != "read" ||
		!errors.Is(projected, cause) || projected.Error() == "" {
		t.Fatalf("repository error projection = %v/%q/%v", projected.Code(), projected.Operation(), projected)
	}

	var nilError *RepositoryError
	if nilError.Error() != "<nil>" || nilError.Unwrap() != nil || nilError.Code() != "" || nilError.Operation() != "" {
		t.Fatal("nil repository error exposed non-zero policy fields")
	}
}

type coverageC6Store struct {
	pinned PinnedInventory
	err    error
	calls  int
}

func (store *coverageC6Store) ListResumeState(context.Context) (PinnedInventory, error) {
	store.calls++
	return store.pinned, store.err
}

type coverageC6ClosingInventory struct {
	PinnedInventory
	close      func() error
	closeCalls int
}

func (inventory *coverageC6ClosingInventory) Close() error {
	inventory.closeCalls++
	return inventory.close()
}

type coverageC6BareLease struct {
	snapshot   RepositorySnapshot
	closeErr   error
	closeCalls int
}

func (lease *coverageC6BareLease) Observe(context.Context) (RepositorySnapshot, error) {
	return lease.snapshot, nil
}

func (*coverageC6BareLease) Apply(context.Context, Action) (ApplyResult, error) {
	return ApplyResult{}, ErrInvalidContract
}

func (lease *coverageC6BareLease) Close() error {
	lease.closeCalls++
	return lease.closeErr
}

type coverageC6PublicationObserverFunc func(context.Context, PinnedCheckpoint) (PinnedPublication, error)

func (function coverageC6PublicationObserverFunc) PinPublication(
	ctx context.Context,
	checkpoint PinnedCheckpoint,
) (PinnedPublication, error) {
	return function(ctx, checkpoint)
}

type coverageC6PublicationPin struct {
	observation PublicationObservation
	closeErr    error
	closeCalls  int
}

func (pin *coverageC6PublicationPin) Observation() PublicationObservation {
	return pin.observation
}

func (pin *coverageC6PublicationPin) Revalidate(context.Context) (Evidence, error) {
	return pin.observation.FinalEvidence(), nil
}

func (pin *coverageC6PublicationPin) Close() error {
	pin.closeCalls++
	return pin.closeErr
}

func coverageC6CanceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

var _ Repository = (*coverageC6Store)(nil)
var _ PinnedInventory = (*coverageC6ClosingInventory)(nil)
var _ LeasedRepository = (*coverageC6BareLease)(nil)
var _ PublicationObserver = coverageC6PublicationObserverFunc(nil)
var _ PinnedPublication = (*coverageC6PublicationPin)(nil)
