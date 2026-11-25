package content

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

type revisionTraceRecorder struct {
	mu     sync.Mutex
	events []RevisionTrace
}

func (r *revisionTraceRecorder) TraceRevision(event RevisionTrace) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *revisionTraceRecorder) snapshot() []RevisionTrace {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RevisionTrace(nil), r.events...)
}

func newObservedCapacityStore(
	t *testing.T,
	pool *HandlePool,
	shareByte byte,
	limits CapacityLimits,
	clock Clock,
	records map[catalog.NodeID]catalog.NodeRecord,
	source RevisionSource,
	cache RevisionCache,
	tracer RevisionTracer,
) *RevisionStore {
	t.Helper()
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](shareByte),
		ChunkSize:     catalog.MinChunkSize,
		Catalog:       testCatalog{records: records},
		Source:        source,
		Clock:         clock,
		LeaseIDs:      &sequenceIDs{},
		HandlePool:    pool,
		ShareLimits:   limits,
		Cache:         cache,
		Tracer:        tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close observed capacity store %d: %v", shareByte, err)
		}
	})
	return store
}

func TestCrossShareProcessLeaseStarvationReapsInactiveStore(t *testing.T) {
	t.Run("new revision admission", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		capacityTraces := &capacityTraceRecorder{}
		pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 2, ActiveLeases: 1}, capacityTraces)
		limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
		victimID, victimRecord := capacityRecord(t, 70)
		requestID, requestRecord := capacityRecord(t, 71)
		victimFile := &capacityStableFile{data: []byte{1}}
		requestFile := &capacityStableFile{data: []byte{2}}
		victimTraces := &revisionTraceRecorder{}

		victimStore := newObservedCapacityStore(t, pool, 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
			nil, victimTraces)
		requestStore := newObservedCapacityStore(t, pool, 2, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)},
			nil, nil)
		victimSession := capacitySession(t, victimStore, "idle-victim", limits)
		requestSession := capacitySession(t, requestStore, "requester", limits)

		victimLease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
		if err != nil {
			t.Fatal(err)
		}
		var busy *CapacityBusyError
		if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSession); !errors.As(err, &busy) ||
			busy.Resource() != CapacityResourceActiveLease || busy.Scope() != CapacityScopeProcess {
			t.Fatalf("unexpired lease pressure = %v", err)
		}
		deniedBeforeExpiry := countCapacityStage(capacityTraces.snapshot(), CapacityTraceDenied)

		clock.Advance(LeaseTTL)
		opened, err := requestStore.OpenRevision(context.Background(), requestID, requestSession)
		if err != nil || opened.ID().IsZero() {
			t.Fatalf("post-expiry new open = (%x, %v)", opened.ID(), err)
		}
		if got := countCapacityStage(capacityTraces.snapshot(), CapacityTraceDenied); got != deniedBeforeExpiry {
			t.Fatalf("spurious denial emitted before cross-store reap: before=%d after=%d", deniedBeforeExpiry, got)
		}
		if victimFile.closed.Load() != 0 {
			t.Fatalf("victim handle within resume grace was prematurely closed (%d)", victimFile.closed.Load())
		}
		if snapshot := requestStore.CapacitySnapshot(); snapshot.Process().Used() != (CapacityUsage{StableHandles: 2, ActiveLeases: 1}) ||
			snapshot.Process().ReclaimableStableHandles() != 1 {
			t.Fatalf("post-reap snapshot = %+v", snapshot)
		}
		if _, err := victimStore.RenewLease(victimLease.ID()); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("reaped victim lease renew = %v, want ErrLeaseExpired", err)
		}
		if !hasRevisionTrace(victimTraces.snapshot(), RevisionTraceStageLeaseSettlement, RevisionTraceCauseDetached) {
			t.Fatalf("missing detached settlement trace on inactive victim: %+v", victimTraces.snapshot())
		}
		if err := requestStore.EndLease(opened.ID(), LeaseRelinquished); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("resident revision reuse", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		capacityTraces := &capacityTraceRecorder{}
		pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 2, ActiveLeases: 2}, capacityTraces)
		limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
		victimID, victimRecord := capacityRecord(t, 72)
		requestID, requestRecord := capacityRecord(t, 73)
		victimFile := &capacityStableFile{data: []byte{1}}
		requestFile := &capacityStableFile{data: []byte{2}}

		victimStore := newObservedCapacityStore(t, pool, 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
			nil, nil)
		requestStore := newObservedCapacityStore(t, pool, 2, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)},
			nil, nil)
		victimSession := capacitySession(t, victimStore, "resident-victim", limits)
		requestSessionA := capacitySession(t, requestStore, "resident-req-a", limits)
		requestSessionB := capacitySession(t, requestStore, "resident-req-b", limits)

		victimLease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(LeaseTTL / 2)
		firstReqLease, err := requestStore.OpenRevision(context.Background(), requestID, requestSessionA)
		if err != nil {
			t.Fatal(err)
		}
		var busy *CapacityBusyError
		if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSessionB); !errors.As(err, &busy) ||
			busy.Resource() != CapacityResourceActiveLease || busy.Scope() != CapacityScopeProcess {
			t.Fatalf("unexpired resident lease pressure = %v", err)
		}
		deniedBeforeExpiry := countCapacityStage(capacityTraces.snapshot(), CapacityTraceDenied)

		// Advance past victim's LeaseTTL while firstReqLease stays unexpired.
		clock.Advance(LeaseTTL / 2)
		secondReqLease, err := requestStore.OpenRevision(context.Background(), requestID, requestSessionB)
		if err != nil || secondReqLease.ID().IsZero() ||
			secondReqLease.Descriptor().FileRevision() != firstReqLease.Descriptor().FileRevision() {
			t.Fatalf("post-expiry resident reuse = (%+v, %v)", secondReqLease, err)
		}
		if got := countCapacityStage(capacityTraces.snapshot(), CapacityTraceDenied); got != deniedBeforeExpiry {
			t.Fatalf("spurious denial emitted on resident cross-store reap: before=%d after=%d", deniedBeforeExpiry, got)
		}
		if _, err := victimStore.RenewLease(victimLease.ID()); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("reaped victim lease renew = %v, want ErrLeaseExpired", err)
		}
		if err := requestStore.EndLease(firstReqLease.ID(), LeaseRelinquished); err != nil {
			t.Fatal(err)
		}
		if err := requestStore.EndLease(secondReqLease.ID(), LeaseRelinquished); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCrossShareProcessHandleStarvationReapsInactiveStore(t *testing.T) {
	tests := []struct {
		name             string
		advance          time.Duration
		wantCleanRelease bool
		wantEvicted      bool
	}{
		{
			name:             "expired lease within resume grace enters idle and evicts",
			advance:          LeaseTTL,
			wantCleanRelease: false,
			wantEvicted:      true,
		},
		{
			name:             "expired lease past resume grace retires cleanly before open",
			advance:          LeaseTTL + RevisionResumeGrace,
			wantCleanRelease: true,
			wantEvicted:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &testClock{now: time.Unix(100, 0)}
			capacityTraces := &capacityTraceRecorder{}
			pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 1, ActiveLeases: 2}, capacityTraces)
			limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
			victimID, victimRecord := capacityRecord(t, 74)
			requestID, requestRecord := capacityRecord(t, 75)
			victimFile := &capacityStableFile{
				data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
			}
			requestFile := &capacityStableFile{data: []byte{2}}
			victimCache := &recordingInvalidator{}
			victimTraces := &revisionTraceRecorder{}
			requestSource := &mappedRevisionSource{
				files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int),
			}

			victimStore := newObservedCapacityStore(t, pool, 1, limits, clock,
				map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
				&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
				victimCache, victimTraces)
			requestStore := newObservedCapacityStore(t, pool, 2, limits, clock,
				map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
				requestSource, nil, nil)
			victimSession := capacitySession(t, victimStore, "handle-victim", limits)
			requestSession := capacitySession(t, requestStore, "handle-requester", limits)

			victimLease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
			if err != nil {
				t.Fatal(err)
			}
			var busy *CapacityBusyError
			if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSession); !errors.As(err, &busy) ||
				busy.Resource() != CapacityResourceStableHandle || busy.Scope() != CapacityScopeProcess {
				t.Fatalf("unexpired handle pressure = %v", err)
			}
			deniedBeforeExpiry := countCapacityStage(capacityTraces.snapshot(), CapacityTraceDenied)

			clock.Advance(test.advance)
			type openResult struct {
				lease RevisionLease
				err   error
			}
			done := make(chan openResult, 1)
			go func() {
				lease, openErr := requestStore.OpenRevision(context.Background(), requestID, requestSession)
				done <- openResult{lease: lease, err: openErr}
			}()

			<-victimFile.closeStarted
			if requestSource.callCount(requestID) != 0 {
				t.Fatal("requester opened replacement handle before inactive victim Close returned")
			}
			close(victimFile.closeRelease)

			opened := <-done
			if opened.err != nil || opened.lease.ID().IsZero() || requestSource.callCount(requestID) != 1 {
				t.Fatalf("post-expiry handle admission = (%x, %v) opens=%d", opened.lease.ID(), opened.err, requestSource.callCount(requestID))
			}
			if victimFile.closed.Load() != 1 {
				t.Fatalf("victim closes = %d, want 1", victimFile.closed.Load())
			}
			if got := countCapacityStage(capacityTraces.snapshot(), CapacityTraceDenied); got != deniedBeforeExpiry {
				t.Fatalf("spurious denial emitted on post-expiry handle admission: before=%d after=%d", deniedBeforeExpiry, got)
			}
			if got := countCapacityStage(capacityTraces.snapshot(), CapacityTraceEvicted) > 0; got != test.wantEvicted {
				t.Fatalf("evicted trace = %v, want %v", got, test.wantEvicted)
			}
			if got := hasRevisionTrace(victimTraces.snapshot(), RevisionTraceStageCleanRelease, RevisionTraceCauseDetached); got != test.wantCleanRelease {
				t.Fatalf("clean release trace = %v, want %v", got, test.wantCleanRelease)
			}
			invalidated, retired := victimCache.snapshot()
			if len(invalidated) != 0 || len(retired) != 1 || retired[0] != victimLease.Descriptor().FileRevision() {
				t.Fatalf("victim cache invalidated=%v retired=%v", invalidated, retired)
			}
			if snapshot := requestStore.CapacitySnapshot(); snapshot.Process().Used() != (CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
				t.Fatalf("post-reap handle snapshot = %+v", snapshot)
			}
			if err := requestStore.EndLease(opened.lease.ID(), LeaseRelinquished); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCrossShareProcessPressureReapsExpiredStoreBeforeEvictingLiveIdleGrace(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 2, ActiveLeases: 4}, nil)
	limits := CapacityLimits{StableHandles: 2, ActiveLeases: 4}
	expiredID, expiredRecord := capacityRecord(t, 76)
	liveIdleID, liveIdleRecord := capacityRecord(t, 77)
	requestID, requestRecord := capacityRecord(t, 78)
	expiredFile := &capacityStableFile{data: []byte{1}}
	liveIdleFile := &capacityStableFile{data: []byte{2}}
	requestFile := &capacityStableFile{data: []byte{3}}
	liveIdleCache := &recordingInvalidator{}

	expiredStore := newObservedCapacityStore(t, pool, 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{expiredID.NodeID(): expiredRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{expiredID: expiredFile}, calls: make(map[catalog.FileID]int)},
		nil, nil)
	liveIdleStore := newObservedCapacityStore(t, pool, 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{liveIdleID.NodeID(): liveIdleRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{liveIdleID: liveIdleFile}, calls: make(map[catalog.FileID]int)},
		liveIdleCache, nil)
	requestStore := newObservedCapacityStore(t, pool, 3, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)},
		nil, nil)

	expiredSession := capacitySession(t, expiredStore, "expired", limits)
	liveIdleSession := capacitySession(t, liveIdleStore, "live-idle", limits)
	requestSession := capacitySession(t, requestStore, "requester", limits)

	if _, err := expiredStore.OpenRevision(context.Background(), expiredID, expiredSession); err != nil {
		t.Fatal(err)
	}
	clock.Advance(LeaseTTL + RevisionResumeGrace)
	liveLease, err := liveIdleStore.OpenRevision(context.Background(), liveIdleID, liveIdleSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := liveIdleStore.EndLease(liveLease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}

	opened, err := requestStore.OpenRevision(context.Background(), requestID, requestSession)
	if err != nil {
		t.Fatalf("open under process pressure with expired sibling = %v", err)
	}
	if expiredFile.closed.Load() != 1 || liveIdleFile.closed.Load() != 0 {
		t.Fatalf("eviction priority expired=%d liveIdle=%d, want 1 and 0", expiredFile.closed.Load(), liveIdleFile.closed.Load())
	}
	if _, retired := liveIdleCache.snapshot(); len(retired) != 0 {
		t.Fatalf("live grace sibling cache was retired: %v", retired)
	}
	if err := requestStore.EndLease(opened.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
}

func TestCrossShareProcessReapJoinsVictimStoreCloseAndRespectsCancellation(t *testing.T) {
	t.Run("victim close waits for cross-store reap close", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 1, ActiveLeases: 2}, nil)
		limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
		victimID, victimRecord := capacityRecord(t, 79)
		requestID, requestRecord := capacityRecord(t, 80)
		victimFile := &capacityStableFile{
			data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
		}
		requestFile := &capacityStableFile{data: []byte{2}}
		victimStore := newObservedCapacityStore(t, pool, 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
			nil, nil)
		requestStore := newObservedCapacityStore(t, pool, 2, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)},
			nil, nil)
		victimSession := capacitySession(t, victimStore, "close-victim", limits)
		requestSession := capacitySession(t, requestStore, "close-requester", limits)

		if _, err := victimStore.OpenRevision(context.Background(), victimID, victimSession); err != nil {
			t.Fatal(err)
		}
		clock.Advance(LeaseTTL + RevisionResumeGrace)

		openErr := make(chan error, 1)
		go func() {
			_, err := requestStore.OpenRevision(context.Background(), requestID, requestSession)
			openErr <- err
		}()
		<-victimFile.closeStarted

		closeErr := make(chan error, 1)
		go func() { closeErr <- victimStore.Close() }()
		select {
		case err := <-closeErr:
			t.Fatalf("victim store Close returned while cross-store reap Close was in flight: %v", err)
		default:
		}
		close(victimFile.closeRelease)
		if err := <-openErr; err != nil {
			t.Fatalf("requester open during victim close = %v", err)
		}
		if err := <-closeErr; err != nil {
			t.Fatalf("victim close during cross-store reap = %v", err)
		}
		if snapshot := requestStore.CapacitySnapshot(); snapshot.Process().Used() != (CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
			t.Fatalf("post-close accounting = %+v", snapshot)
		}
	})

	t.Run("requester cancellation during cross-store reap close leaks nothing", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 1, ActiveLeases: 2}, nil)
		limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
		victimID, victimRecord := capacityRecord(t, 81)
		requestID, requestRecord := capacityRecord(t, 82)
		victimFile := &capacityStableFile{
			data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
		}
		requestSource := &mappedRevisionSource{
			files: map[catalog.FileID]StableFile{requestID: &capacityStableFile{data: []byte{2}}},
			calls: make(map[catalog.FileID]int),
		}
		victimStore := newObservedCapacityStore(t, pool, 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
			nil, nil)
		requestStore := newObservedCapacityStore(t, pool, 2, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
			requestSource, nil, nil)
		victimSession := capacitySession(t, victimStore, "cancel-reap-victim", limits)
		requestSession := capacitySession(t, requestStore, "cancel-reap-requester", limits)

		if _, err := victimStore.OpenRevision(context.Background(), victimID, victimSession); err != nil {
			t.Fatal(err)
		}
		clock.Advance(LeaseTTL + RevisionResumeGrace)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := requestStore.OpenRevision(ctx, requestID, requestSession)
			result <- err
		}()
		<-victimFile.closeStarted
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cross-store reap = %v", err)
		}
		close(victimFile.closeRelease)
		requestStore.openWG.Wait()
		if requestSource.callCount(requestID) != 0 || victimFile.closed.Load() != 1 ||
			requestStore.CapacitySnapshot().Process().Used() != (CapacityUsage{}) {
			t.Fatalf("cancelled reap opens=%d victimCloses=%d snapshot=%+v",
				requestSource.callCount(requestID), victimFile.closed.Load(), requestStore.CapacitySnapshot())
		}
	})
}

func TestCrossShareProcessReapNeverClosesActiveReader(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 1, ActiveLeases: 1}, nil)
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	victimID, victimRecord := capacityRecord(t, 83)
	requestID, requestRecord := capacityRecord(t, 84)
	victimFile := &capacityStableFile{
		data: []byte{1}, readStarted: make(chan struct{}), readRelease: make(chan struct{}),
	}
	requestFile := &capacityStableFile{data: []byte{2}}
	victimCache := &recordingInvalidator{}
	victimStore := newObservedCapacityStore(t, pool, 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
		victimCache, nil)
	requestStore := newObservedCapacityStore(t, pool, 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)},
		nil, nil)
	victimSession := capacitySession(t, victimStore, "reader-victim", limits)
	requestSession := capacitySession(t, requestStore, "reader-requester", limits)

	victimLease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := NewBlockRef(victimID, victimLease.Descriptor().FileRevision(), 0, victimLease.Descriptor().Geometry())
	readErr := make(chan error, 1)
	go func() {
		_, err := victimStore.ReadBlock(context.Background(), victimLease.ID(), ref)
		readErr <- err
	}()
	<-victimFile.readStarted

	clock.Advance(LeaseTTL + RevisionResumeGrace)
	var busy *CapacityBusyError
	if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSession); !errors.As(err, &busy) ||
		busy.Resource() != CapacityResourceStableHandle || busy.Scope() != CapacityScopeProcess {
		t.Fatalf("pressure during expired active reader = %v", err)
	}
	if victimFile.closed.Load() != 0 || victimFile.closeDuringRead.Load() {
		t.Fatal("cross-store reap closed a handle with an active reader")
	}
	if _, retired := victimCache.snapshot(); len(retired) != 1 {
		t.Fatalf("expired retired revision did not retire cache: %v", retired)
	}

	close(victimFile.readRelease)
	if err := <-readErr; err != nil {
		t.Fatalf("in-flight read error = %v", err)
	}
	if victimFile.closed.Load() != 1 || victimFile.closeDuringRead.Load() {
		t.Fatalf("post-read close state closes=%d during=%v", victimFile.closed.Load(), victimFile.closeDuringRead.Load())
	}
	opened, err := requestStore.OpenRevision(context.Background(), requestID, requestSession)
	if err != nil || opened.ID().IsZero() {
		t.Fatalf("post-reader admission = (%x, %v)", opened.ID(), err)
	}
	if err := requestStore.EndLease(opened.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
}

func TestCrossShareProcessReapUsesEachStoresClock(t *testing.T) {
	victimClock := &testClock{now: time.Unix(100, 0)}
	requestClock := &testClock{now: time.Unix(100, 0)}
	pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 1, ActiveLeases: 1}, nil)
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	victimID, victimRecord := capacityRecord(t, 85)
	requestID, requestRecord := capacityRecord(t, 86)
	victimFile := &capacityStableFile{data: []byte{1}}
	requestFile := &capacityStableFile{data: []byte{2}}

	victimStore := newObservedCapacityStore(t, pool, 1, limits, victimClock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victimFile}, calls: make(map[catalog.FileID]int)},
		nil, nil)
	requestStore := newObservedCapacityStore(t, pool, 2, limits, requestClock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)},
		nil, nil)
	victimSession := capacitySession(t, victimStore, "clock-victim", limits)
	requestSession := capacitySession(t, requestStore, "clock-requester", limits)

	if _, err := victimStore.OpenRevision(context.Background(), victimID, victimSession); err != nil {
		t.Fatal(err)
	}
	victimClock.Advance(LeaseTTL + RevisionResumeGrace)

	opened, err := requestStore.OpenRevision(context.Background(), requestID, requestSession)
	if err != nil || opened.ID().IsZero() || victimFile.closed.Load() != 1 {
		t.Fatalf("per-store clock reap = (%x, %v) victimCloses=%d", opened.ID(), err, victimFile.closed.Load())
	}
	if err := requestStore.EndLease(opened.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
}

func countCapacityStage(events []CapacityTraceEvent, stage CapacityTraceStage) int {
	count := 0
	for _, event := range events {
		if event.Stage() == stage {
			count++
		}
	}
	return count
}

func hasRevisionTrace(events []RevisionTrace, stage RevisionTraceStage, cause RevisionTraceCause) bool {
	for _, event := range events {
		if event.Stage() == stage && event.Cause() == cause {
			return true
		}
	}
	return false
}
