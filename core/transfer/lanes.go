package transfer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

const (
	MaxLogicalLanes       = 16
	DefaultLaneRaceWidth  = 1
	MaxDemandLaneAttempts = MaxLogicalLanes
	maximumLaneFailures   = 1_000
)

var (
	ErrInvalidLane     = errors.New("transfer lane is invalid")
	ErrStaleLane       = errors.New("transfer lane epoch is stale")
	ErrLaneBudget      = errors.New("transfer lane budget exceeded")
	ErrLaneClosed      = errors.New("transfer lane set is closed")
	errBlockSuperseded = errors.New("block attempt superseded by an authenticated winner")
)

type LaneIdentity struct {
	ID    uint32
	Epoch uint32
}

type BlockDemand struct {
	LeaseID    content.LeaseID
	Descriptor content.FileRevisionDescriptor
	Index      uint64
}

type BlockLane interface {
	// Receipt reports unique sealed-object bytes before authentication. It may
	// replenish network supply, but never proves output durability.
	FetchBlock(context.Context, BlockDemand, func(uint64)) (records.BlockRecord, error)
}

// BlockAttemptSuperseded proves that the demand owner canceled a redundant
// attempt after selecting an authenticated winner. Plain cancellation, including
// a parent failure, cannot supply this evidence to a lane's terminal diagnostics.
func BlockAttemptSuperseded(ctx context.Context) bool {
	return ctx != nil && context.Cause(ctx) == errBlockSuperseded //nolint:errorlint // Only the lane round's exact cause grants supersession ownership.
}

type LaneSetConfig struct {
	ContentRoutePolicy            ContentRoutePolicy
	ProtocolSessionID             protocolsession.ProtocolSessionID
	RaceWidth                     int
	Now                           func() time.Time
	SettlementObservationCapacity LaneSettlementObservationCapacity
}

type laneState struct {
	identity        LaneIdentity
	route           LaneRoute
	lane            BlockLane
	inflight        uint32
	settlementHolds uint32
	failures        uint32
	performance     lanescheduling.Performance
	receive         lanescheduling.ReceiveWindow
	retired         bool
	settled         bool
	settlement      *laneSettlementCounters
	usefulBytes     uint64
	lastUsefulAt    time.Time
	admittedAt      time.Time
}

type contentLaneSuspensionPolicy struct {
	laneID  uint32
	resumed bool
}

// ContentLaneSuspension is an epoch-stable content-admission capability for
// one authenticated logical lane. Its opaque policy identity prevents an old
// handle from releasing a newer hold on the same lane ID.
type ContentLaneSuspension struct {
	lanes  *LaneSet
	policy *contentLaneSuspensionPolicy
}

type LaneSet struct {
	contentRoutePolicy ContentRoutePolicy
	sessionID          protocolsession.ProtocolSessionID
	raceWidth          int
	now                func() time.Time

	lifecycle context.Context
	stop      context.CancelFunc

	mu                  sync.Mutex
	attempts            sync.WaitGroup
	fetches             sync.WaitGroup
	publications        sync.WaitGroup
	closeStarted        bool
	closeDone           chan struct{}
	closed              bool
	lanes               map[uint32]*laneState
	contentSuspensions  map[uint32]*contentLaneSuspensionPolicy
	cursor              uint64
	exploration         lanescheduling.Exploration
	availabilityChanged chan struct{}
	settlementProducer  observationstream.Producer[LaneSettlementSummary]
	settlementConsumer  observationstream.Consumer[LaneSettlementSummary]
	finalSettlements    []*laneState
	contentActivity     [LaneRouteTURN + 1]LaneContentActivity
	downloadMetrics     *downloadmetrics.Metrics
}

func NewLaneSet(config LaneSetConfig) (*LaneSet, error) {
	if !config.ContentRoutePolicy.valid() {
		return nil, ErrInvalidLane
	}
	if config.ProtocolSessionID.IsZero() {
		return nil, errors.New("lane set requires a protocol session identity")
	}
	if config.RaceWidth == 0 {
		config.RaceWidth = DefaultLaneRaceWidth
	}
	if config.RaceWidth < 1 || config.RaceWidth > MaxLogicalLanes {
		return nil, errors.New("lane race width is outside the logical lane limit")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	var settlementProducer observationstream.Producer[LaneSettlementSummary]
	var settlementConsumer observationstream.Consumer[LaneSettlementSummary]
	if config.SettlementObservationCapacity != 0 {
		var err error
		settlementProducer, settlementConsumer, err = observationstream.New[LaneSettlementSummary](
			observationstream.Capacity(config.SettlementObservationCapacity),
		)
		if err != nil {
			return nil, fmt.Errorf("lane settlement observations: %w", err)
		}
	}
	lifecycle, stop := context.WithCancel(context.Background())
	return &LaneSet{
		contentRoutePolicy: config.ContentRoutePolicy,
		sessionID:          config.ProtocolSessionID, raceWidth: config.RaceWidth, now: config.Now,
		lifecycle: lifecycle, stop: stop, lanes: make(map[uint32]*laneState),
		contentSuspensions:  make(map[uint32]*contentLaneSuspensionPolicy),
		availabilityChanged: make(chan struct{}),
		settlementProducer:  settlementProducer,
		settlementConsumer:  settlementConsumer,
		closeDone:           make(chan struct{}),
	}, nil
}

func (s *LaneSet) Add(identity LaneIdentity, route LaneRoute, lane BlockLane) error {
	if identity.ID == 0 || !route.valid() || lane == nil {
		return ErrInvalidLane
	}
	state := &laneState{identity: identity, route: route, lane: lane, admittedAt: s.now()}
	if s.settlementObservationsEnabled() {
		state.settlement = &laneSettlementCounters{}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrLaneClosed
	}
	var retired *LaneSettlementSummary
	if current := s.lanes[identity.ID]; current != nil {
		if identity.Epoch <= current.identity.Epoch {
			s.mu.Unlock()
			return ErrStaleLane
		}
		retired = s.retireLaneLocked(current)
		if retired != nil {
			s.publications.Add(1)
		}
		s.lanes[identity.ID] = state
		s.notifyAvailabilityLocked()
		s.mu.Unlock()
		s.publishRegisteredLaneSettlement(retired)
		return nil
	}
	if _, reattachingHeldLane := s.contentSuspensions[identity.ID]; !reattachingHeldLane && s.logicalLaneCountLocked() == MaxLogicalLanes {
		s.mu.Unlock()
		return ErrLaneBudget
	}
	s.lanes[identity.ID] = state
	s.notifyAvailabilityLocked()
	s.mu.Unlock()
	return nil
}

// SettlementObservations returns nil when settlement observation was disabled.
// The receive-only capability leaves publication and completion with LaneSet.
func (s *LaneSet) SettlementObservations() observationstream.Consumer[LaneSettlementSummary] {
	if s == nil {
		return nil
	}
	return s.settlementConsumer
}

func (s *LaneSet) settlementObservationsEnabled() bool {
	return s != nil && s.settlementConsumer != nil
}

func (s *LaneSet) Remove(identity LaneIdentity) bool {
	if identity.ID == 0 {
		return false
	}
	s.mu.Lock()
	current := s.lanes[identity.ID]
	if current == nil || current.identity != identity {
		s.mu.Unlock()
		return false
	}
	delete(s.lanes, identity.ID)
	retired := s.retireLaneLocked(current)
	if retired != nil {
		s.publications.Add(1)
	}
	s.notifyAvailabilityLocked()
	s.mu.Unlock()
	s.publishRegisteredLaneSettlement(retired)
	return true
}

func (s *LaneSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lanes)
}

type laneAdmission struct {
	*laneState
	permit  *lanescheduling.ReceivePermit
	release func()
}

type laneCandidate struct {
	state *laneState
	order uint64
}

func (s *LaneSet) candidates(
	ctx context.Context,
	attempted map[LaneIdentity]struct{},
	bytes uint64,
	claim blockClaim,
) ([]*laneAdmission, bool, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, false, ErrLaneClosed
		}
		remaining := MaxDemandLaneAttempts - len(attempted)
		if remaining <= 0 {
			s.mu.Unlock()
			return nil, true, nil
		}
		ordered := make([]*laneState, 0, len(s.lanes))
		untriedSuspended := false
		for _, state := range s.lanes {
			if _, alreadyAttempted := attempted[state.identity]; alreadyAttempted {
				continue
			}
			if _, suspended := s.contentSuspensions[state.identity.ID]; suspended || !s.contentRoutePolicy.Allows(state.route) {
				untriedSuspended = true
				continue
			}
			ordered = append(ordered, state)
		}
		var capacityChanged <-chan struct{}
		if len(ordered) != 0 {
			selected, changed, err := s.selectCandidatesLocked(ordered, remaining, bytes, claim)
			capacityChanged = changed
			if err != nil {
				s.mu.Unlock()
				return nil, false, err
			}
			// Registration remains inside the closed-state lock so Close cannot
			// observe a zero group while an admitted hedge is about to start.
			if len(selected) > 0 {
				s.attempts.Add(len(selected))
				s.mu.Unlock()
				return selected, false, nil
			}
		}
		waitForFirstLane := len(s.lanes) == 0 && len(attempted) == 0
		if !waitForFirstLane && !untriedSuspended && len(ordered) == 0 {
			s.mu.Unlock()
			return nil, true, nil
		}
		changed := s.availabilityChanged
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-s.lifecycle.Done():
			return nil, false, ErrLaneClosed
		case <-changed:
		case <-capacityChanged:
		}
	}
}

func (s *LaneSet) selectCandidatesLocked(ordered []*laneState, remaining int, bytes uint64, claim blockClaim) ([]*laneAdmission, <-chan struct{}, error) {
	slices.SortFunc(ordered, func(left, right *laneState) int {
		return cmp.Compare(left.identity.ID, right.identity.ID)
	})
	start := int(s.cursor % uint64(len(ordered)))
	s.cursor++
	candidates := make([]laneCandidate, 0, len(ordered))
	for rank := range ordered {
		state := ordered[(start+rank)%len(ordered)]
		candidates = append(candidates, laneCandidate{state: state, order: uint64(rank)})
	}
	slices.SortFunc(candidates, func(left, right laneCandidate) int {
		if left.state.failures != right.state.failures {
			return cmp.Compare(left.state.failures, right.state.failures)
		}
		// A canceled probe only bounds speed; it cannot prove that a standby can
		// finish content before the output window fills.
		if left.state.performance.HasSuccessfulSample != right.state.performance.HasSuccessfulSample {
			if left.state.performance.HasSuccessfulSample {
				return -1
			}
			return 1
		}
		if compared := cmp.Compare(laneCompletionCost(left.state, bytes), laneCompletionCost(right.state, bytes)); compared != 0 {
			return compared
		}
		return cmp.Compare(left.order, right.order)
	})
	limit := min(s.raceWidth, len(candidates), remaining)
	selected := make([]*laneAdmission, 0, limit)
	for _, candidate := range candidates {
		if len(selected) == limit {
			break
		}
		admission, changed, err := s.admitLaneLocked(candidate.state, bytes, claim)
		if admission != nil {
			selected = append(selected, admission)
		} else if changed == nil && err == nil {
			// A busy preferred path is not evidence that a slower standby should
			// own the output frontier. Bounded probes evaluate that alternative.
			break
		}
		if changed != nil || err != nil {
			if len(selected) > 0 {
				return selected, nil, nil
			}
			return nil, changed, err
		}
	}
	return selected, nil, nil
}

func (s *LaneSet) admitLaneLocked(state *laneState, bytes uint64, claim blockClaim) (*laneAdmission, <-chan struct{}, error) {
	if !state.receive.Available() {
		return nil, nil, nil
	}
	var release func()
	if claim != nil {
		var changed <-chan struct{}
		var err error
		release, changed, err = claim()
		if release == nil {
			return nil, changed, err
		}
	}
	permit := state.receive.TryReserve(max(1, bytes), s.now())
	if permit == nil {
		if release != nil {
			release()
		}
		return nil, nil, nil
	}
	state.inflight++
	state.performance.Begin(s.now(), bytes)
	return &laneAdmission{laneState: state, permit: permit, release: release}, nil, nil
}

func laneCompletionCost(state *laneState, bytes uint64) float64 {
	factor := 1.0
	if state.route == LaneRouteTURN {
		factor = lanescheduling.UserRelayCostFactor
	}
	if state.route == LaneRouteRelay {
		factor = lanescheduling.RelayCostFactor
	}
	return lanescheduling.Cost(state.performance.Estimate(bytes), factor)
}

func blockDemandBytes(demand BlockDemand) uint64 {
	bytes, _ := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
	return uint64(bytes)
}

// Supplemental reads race the same immutable block. They never take exclusive
// ownership of a new prefix block and cannot bypass content-route authority.
func (s *LaneSet) supplement(
	demand BlockDemand, attempted map[LaneIdentity]struct{}, primary *laneState,
	elapsed, estimate time.Duration, purpose lanescheduling.Purpose, claim blockClaim,
) *laneAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(attempted) >= MaxDemandLaneAttempts {
		return nil
	}
	now, bytes := s.now(), blockDemandBytes(demand)
	if purpose == lanescheduling.Probe && !primary.performance.HasSuccessfulSample {
		return nil
	}
	best := s.supplementCandidateLocked(attempted, bytes, purpose, now)
	if best == nil {
		return nil
	}
	if purpose == lanescheduling.Rescue && !lanescheduling.RescueDue(elapsed, estimate, best.performance.Estimate(bytes)) {
		return nil
	}
	if !s.exploration.Acquire(purpose, now) {
		return nil
	}
	admission, _, _ := s.admitLaneLocked(best, bytes, claim)
	if admission == nil {
		s.exploration.Release(purpose)
		return nil
	}
	attempted[best.identity] = struct{}{}
	s.attempts.Add(1)
	return admission
}

func (s *LaneSet) supplementCandidateLocked(attempted map[LaneIdentity]struct{}, bytes uint64,
	purpose lanescheduling.Purpose, now time.Time) *laneState {
	var best *laneState
	for _, state := range s.lanes {
		if _, tried := attempted[state.identity]; tried {
			continue
		}
		if _, suspended := s.contentSuspensions[state.identity.ID]; suspended || !s.contentRoutePolicy.Allows(state.route) {
			continue
		}
		if !state.receive.Available() {
			continue
		}
		if purpose == lanescheduling.Probe && !lanescheduling.ProbeDue(&state.performance, now) {
			continue
		}
		if best == nil {
			best = state
			continue
		}
		if purpose == lanescheduling.Probe {
			// Oldest evidence first prevents a previously sampled fallback from
			// monopolizing the exploration budget when a new path appears.
			if state.performance.LastAttempt.Before(best.performance.LastAttempt) ||
				(state.performance.LastAttempt.Equal(best.performance.LastAttempt) && state.identity.ID < best.identity.ID) {
				best = state
			}
		} else if laneCompletionCost(state, bytes) < laneCompletionCost(best, bytes) ||
			(laneCompletionCost(state, bytes) == laneCompletionCost(best, bytes) && state.identity.ID < best.identity.ID) {
			best = state
		}
	}
	return best
}

func (s *LaneSet) notifyAvailabilityLocked() {
	s.observeDownloadAvailabilityLocked()
	close(s.availabilityChanged)
	s.availabilityChanged = make(chan struct{})
}

type laneResult struct {
	state        *laneState
	record       records.BlockRecord
	err          error
	normalized   *lifecycleFailure
	notAdmitted  bool
	reassignable bool
}

type laneRoundKind uint8

const (
	laneRoundSucceeded laneRoundKind = iota + 1
	laneRoundFailed
	laneRoundInterrupted
)

type laneRoundResult struct {
	kind     laneRoundKind
	record   authenticatedBlock
	failures []laneResult
	err      error
}

type laneRoundDecision struct {
	done   chan struct{}
	winner *laneState
}

func (s *LaneSet) fetch(
	ctx context.Context,
	request blockRequest,
) (authenticatedBlock, error) {
	demand, validate := request.demand, request.validate
	if err := ctx.Err(); err != nil {
		return authenticatedBlock{}, err
	}
	if !s.beginFetch() {
		return authenticatedBlock{}, ErrLaneClosed
	}
	defer s.fetches.Done()
	attempted := make(map[LaneIdentity]struct{}, MaxDemandLaneAttempts)
	failures := laneFailureSet{}
	var pendingReassignments []laneResult
	supplemented := false
	defer func() {
		s.resolveLaneReassignments(pendingReassignments, false)
	}()
	for len(attempted) < MaxDemandLaneAttempts {
		candidates, exhausted, err := s.candidates(ctx, attempted, blockDemandBytes(demand), request.claim)
		if err != nil {
			normalized := admitInternalFailure(normalizeSourceBoundary(ctx, err))
			return authenticatedBlock{}, collaboratorError(normalized, err)
		}
		if exhausted {
			return authenticatedBlock{}, collaboratorError(failures.failure, failures.diagnostic)
		}
		// Reassignment is an admitted action, not an inference from a retryable
		// error. Candidate selection has already reserved the subsequent round.
		s.resolveLaneReassignments(pendingReassignments, true)
		pendingReassignments = nil
		for _, state := range candidates {
			attempted[state.identity] = struct{}{}
		}
		round := s.runLaneRound(ctx, demand, validate, candidates, attempted, &supplemented, request.claim)
		switch round.kind {
		case laneRoundSucceeded:
			return round.record, nil
		case laneRoundInterrupted:
			return authenticatedBlock{}, round.err
		case laneRoundFailed:
			var reassignable bool
			failures, reassignable = reduceLaneFailures(failures, round.failures)
			if !reassignable {
				return authenticatedBlock{}, collaboratorError(failures.failure, failures.diagnostic)
			}
			pendingReassignments = round.failures
		}
	}
	return authenticatedBlock{}, collaboratorError(failures.failure, failures.diagnostic)
}

func (s *LaneSet) beginFetch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	// Registration shares the closed-state lock with Stop so Close cannot begin
	// a join while an admitted fetch is still about to select its first round.
	s.fetches.Add(1)
	return true
}

func (s *LaneSet) runLaneRound(
	ctx context.Context,
	demand BlockDemand,
	validate func(records.BlockRecord) error,
	candidates []*laneAdmission,
	attempted map[LaneIdentity]struct{},
	supplemented *bool,
	claim blockClaim,
) laneRoundResult {
	raceContext, cancel := context.WithCancelCause(ctx)
	stopLifecycle := context.AfterFunc(s.lifecycle, func() { cancel(ErrLaneClosed) })
	decision := &laneRoundDecision{done: make(chan struct{})}
	defer func() {
		close(decision.done)
		stopLifecycle()
		if decision.winner != nil {
			cancel(errBlockSuperseded)
		} else {
			cancel(nil)
		}
	}()
	results := make(chan laneResult, MaxDemandLaneAttempts)
	for _, state := range candidates {
		go s.fetchLane(raceContext, demand, validate, state, decision, results, lanescheduling.Content)
	}
	started := s.now()
	s.mu.Lock()
	estimate := candidates[0].performance.Estimate(0)
	s.mu.Unlock()
	active := len(candidates)
	startSupplement := func(purpose lanescheduling.Purpose) {
		if *supplemented || len(candidates) != 1 || raceContext.Err() != nil {
			return
		}
		state := s.supplement(demand, attempted, candidates[0].laneState, s.now().Sub(started), estimate, purpose, claim)
		if state == nil {
			return
		}
		*supplemented = true
		active++
		go s.fetchLane(raceContext, demand, validate, state, decision, results, purpose)
	}
	startSupplement(lanescheduling.Probe)
	ticker := time.NewTicker(lanescheduling.HedgeCheckInterval)
	defer ticker.Stop()
	failures := make([]laneResult, 0, active)
	for active > 0 {
		select {
		case <-raceContext.Done():
			return interruptedLaneRound(ctx)
		case <-ticker.C:
			startSupplement(lanescheduling.Rescue)
		case result := <-results:
			// Cancellation and its lane result can become ready together. The
			// demand owner determines the outcome, not select's ready-case choice.
			if raceContext.Err() != nil {
				return interruptedLaneRound(ctx)
			}
			active--
			if result.err == nil {
				decision.winner = result.state
				return laneRoundResult{kind: laneRoundSucceeded, record: s.attestBlock(result.state, result.record)}
			}
			failures = append(failures, result)
		}
	}
	if laneResultsReassignable(failures) {
		s.holdLaneReassignments(failures)
	}
	return laneRoundResult{kind: laneRoundFailed, failures: failures}
}

func interruptedLaneRound(ctx context.Context) laneRoundResult {
	err := ctx.Err()
	if err == nil {
		err = ErrLaneClosed
	}
	return laneRoundResult{kind: laneRoundInterrupted, err: err}
}
