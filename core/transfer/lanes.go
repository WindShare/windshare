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
)

const (
	MaxLogicalLanes       = 16
	DefaultLaneRaceWidth  = 1
	MaxDemandLaneAttempts = MaxLogicalLanes
	maximumLaneFailures   = 1_000
)

var (
	ErrInvalidLane = errors.New("transfer lane is invalid")
	ErrStaleLane   = errors.New("transfer lane epoch is stale")
	ErrLaneBudget  = errors.New("transfer lane budget exceeded")
	ErrLaneClosed  = errors.New("transfer lane set is closed")
)

// demandNotAdmittedError is an opaque concrete capability proving that a lane
// failed before its operation reached a transport. It requires explicit
// construction through NewDemandNotAdmitted; accepting an incidental public
// marker would let unrelated errors accidentally authorize duplicate work.
type demandNotAdmittedError struct{ cause error }

func NewDemandNotAdmitted(cause error) error {
	if cause == nil {
		cause = ErrInvalidLane
	}
	return &demandNotAdmittedError{cause: cause}
}

func (e *demandNotAdmittedError) Error() string {
	return fmt.Sprintf("demand was not admitted: %v", e.cause)
}
func (e *demandNotAdmittedError) Unwrap() error { return e.cause }

func isDemandNotAdmitted(err error) bool {
	var notAdmitted *demandNotAdmittedError
	return errors.As(err, &notAdmitted) && notAdmitted != nil
}

// demandReassignableAfterRetirementError is an opaque proof that an admitted
// block operation has acquired its exact-generation cancellation tombstone.
// Block reads are immutable under their revision lease, so a different lane may
// now issue a fresh operation without aliasing responses from the retired one.
type demandReassignableAfterRetirementError struct{ cause error }

func NewDemandReassignableAfterRetirement(cause error) error {
	if cause == nil {
		cause = ErrInvalidLane
	}
	return &demandReassignableAfterRetirementError{cause: cause}
}

func (e *demandReassignableAfterRetirementError) Error() string {
	return fmt.Sprintf("demand is reassignable after retiring its admitted operation: %v", e.cause)
}
func (e *demandReassignableAfterRetirementError) Unwrap() error { return e.cause }

func isDemandReassignableAfterRetirement(err error) bool {
	var retired *demandReassignableAfterRetirementError
	return errors.As(err, &retired) && retired != nil
}

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
	FetchBlock(context.Context, BlockDemand) (records.BlockRecord, error)
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
	latency         time.Duration
	retired         bool
	settled         bool
	settlement      *laneSettlementCounters
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
	state := &laneState{identity: identity, route: route, lane: lane}
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

func (s *LaneSet) logicalLaneCountLocked() int {
	count := len(s.lanes)
	for laneID := range s.contentSuspensions {
		if s.lanes[laneID] == nil {
			count++
		}
	}
	return count
}

// SuspendContent removes an authenticated logical lane from content admission
// without detaching its control transport. The initial exact identity prevents
// suspending an unintended incarnation, while the returned capability follows
// replacements because reconnects must not bypass an active admission policy.
func (s *LaneSet) SuspendContent(identity LaneIdentity) (*ContentLaneSuspension, error) {
	if identity.ID == 0 {
		return nil, ErrInvalidLane
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrLaneClosed
	}
	state := s.lanes[identity.ID]
	if state == nil || state.identity != identity {
		return nil, ErrStaleLane
	}
	if _, exists := s.contentSuspensions[identity.ID]; exists {
		return nil, ErrInvalidLane
	}
	policy := &contentLaneSuspensionPolicy{laneID: identity.ID}
	s.contentSuspensions[identity.ID] = policy
	s.notifyAvailabilityLocked()
	return &ContentLaneSuspension{lanes: s, policy: policy}, nil
}

// Resume releases only the hold represented by this capability. It is
// idempotent so concurrent admission signals cannot release a later policy.
func (suspension *ContentLaneSuspension) Resume() error {
	if suspension == nil || suspension.lanes == nil || suspension.policy == nil {
		return ErrInvalidLane
	}
	lanes := suspension.lanes
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	if suspension.policy.resumed {
		return nil
	}
	if lanes.closed {
		return ErrLaneClosed
	}
	current := lanes.contentSuspensions[suspension.policy.laneID]
	if current != suspension.policy {
		return ErrStaleLane
	}
	suspension.policy.resumed = true
	delete(lanes.contentSuspensions, suspension.policy.laneID)
	lanes.notifyAvailabilityLocked()
	return nil
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

type laneCandidate struct {
	state *laneState
	order uint64
}

func (s *LaneSet) candidates(
	ctx context.Context,
	attempted map[LaneIdentity]struct{},
) ([]*laneState, bool, error) {
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
		if len(ordered) != 0 {
			selected := s.selectCandidatesLocked(ordered, remaining)
			// Registration remains inside the closed-state lock so Close cannot
			// observe a zero group while an admitted hedge is about to start.
			s.attempts.Add(len(selected))
			s.mu.Unlock()
			return selected, false, nil
		}
		waitForFirstLane := len(s.lanes) == 0 && len(attempted) == 0
		if !waitForFirstLane && !untriedSuspended {
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
		}
	}
}

func (s *LaneSet) selectCandidatesLocked(ordered []*laneState, remaining int) []*laneState {
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
		if left.state.inflight != right.state.inflight {
			return cmp.Compare(left.state.inflight, right.state.inflight)
		}
		// Rotation precedes latency so every healthy lane receives bounded
		// progress. The race itself still lets the fastest selected lane win;
		// historical speed must not permanently starve a slower fallback.
		if left.order != right.order {
			return cmp.Compare(left.order, right.order)
		}
		return cmp.Compare(left.state.latency, right.state.latency)
	})
	// Epoch churn can expose a fresh identity for every logical lane after earlier
	// attempts. The per-demand budget applies to identities, not the current map
	// width, so the final hedge batch must be clipped to the remaining authority.
	limit := min(s.raceWidth, len(candidates), remaining)
	selected := make([]*laneState, limit)
	for index := range selected {
		selected[index] = candidates[index].state
		selected[index].inflight++
	}
	return selected
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

type laneFailureSet struct {
	failure    *lifecycleFailure
	diagnostic error
}

func (s *LaneSet) fetch(
	ctx context.Context,
	demand BlockDemand,
	validate func(records.BlockRecord) error,
) (authenticatedBlock, error) {
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
	defer func() {
		s.resolveLaneReassignments(pendingReassignments, false)
	}()
	for len(attempted) < MaxDemandLaneAttempts {
		candidates, exhausted, err := s.candidates(ctx, attempted)
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
		round := s.runLaneRound(ctx, demand, validate, candidates)
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
	candidates []*laneState,
) laneRoundResult {
	raceContext, cancel := context.WithCancel(ctx)
	stopLifecycle := context.AfterFunc(s.lifecycle, cancel)
	decision := &laneRoundDecision{done: make(chan struct{})}
	defer func() {
		close(decision.done)
		stopLifecycle()
		cancel()
	}()

	results := make(chan laneResult, len(candidates))
	for _, state := range candidates {
		go s.fetchLane(raceContext, demand, validate, state, decision, results)
	}
	failures := make([]laneResult, 0, len(candidates))
	for range candidates {
		select {
		case <-raceContext.Done():
			if ctx.Err() != nil {
				return laneRoundResult{kind: laneRoundInterrupted, err: ctx.Err()}
			}
			return laneRoundResult{kind: laneRoundInterrupted, err: ErrLaneClosed}
		case result := <-results:
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

func laneResultsReassignable(results []laneResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if !result.reassignable {
			return false
		}
	}
	return true
}

func (s *LaneSet) fetchLane(
	ctx context.Context,
	demand BlockDemand,
	validate func(records.BlockRecord) error,
	state *laneState,
	decision *laneRoundDecision,
	results chan<- laneResult,
) {
	defer s.attempts.Done()
	started := s.now()
	record, fetchErr := state.lane.FetchBlock(ctx, demand)
	if fetchErr == nil {
		fetchErr = validate(record)
	}
	notAdmitted := isDemandNotAdmitted(fetchErr)
	normalized := admitInternalFailure(normalizeSourceBoundary(ctx, fetchErr))
	canceled := normalized != nil && normalized.policy.canceled
	reassignable := !canceled && (notAdmitted || isDemandReassignableAfterRetirement(fetchErr))
	elapsed := s.now().Sub(started)
	results <- laneResult{
		state: state, record: record, err: fetchErr,
		normalized: normalized, notAdmitted: notAdmitted, reassignable: reassignable,
	}
	<-decision.done
	s.finish(state, elapsed, record, fetchErr, canceled, decision.winner == state)
}

func reduceLaneFailures(current laneFailureSet, results []laneResult) (laneFailureSet, bool) {
	ordered := slices.Clone(results)
	slices.SortFunc(ordered, func(left, right laneResult) int {
		if compared := cmp.Compare(left.state.identity.ID, right.state.identity.ID); compared != 0 {
			return compared
		}
		return cmp.Compare(left.state.identity.Epoch, right.state.identity.Epoch)
	})
	reassignable := true
	for _, result := range ordered {
		diagnostic := fmt.Errorf(
			"lane %d/%d: %w", result.state.identity.ID, result.state.identity.Epoch, result.err,
		)
		failure := result.normalized
		if result.notAdmitted {
			failure = sourceUnavailableFailure(diagnostic)
		}
		current.failure = joinClosedLifecycleFailures(current.failure, failure)
		current.diagnostic = errors.Join(current.diagnostic, diagnostic)
		reassignable = reassignable && result.reassignable
	}
	return current, reassignable
}

func (state *laneState) recordFailure(canceled bool) {
	if canceled {
		return
	}
	if state.failures < maximumLaneFailures {
		state.failures++
	}
	state.settlement.addFailure()
}

func (state *laneState) recordSuccess(elapsed time.Duration) {
	state.failures = 0
	if elapsed < 0 {
		elapsed = 0
	}
	if state.latency == 0 {
		state.latency = elapsed
		return
	}
	state.latency = (state.latency*3 + elapsed) / 4
}
