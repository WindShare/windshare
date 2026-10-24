package lanes

import (
	"context"
	"log/slog"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

// The host's wake cadence is separate from the shared rescue eligibility rule.
const rescuePollInterval = 100 * time.Millisecond

type laneResult struct {
	state        *laneState
	record       records.BlockRecord
	err          error
	normalized   *failurestate.Failure
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
	record   Block
	failures []laneResult
	err      error
}

type laneRoundDecision struct {
	done   chan struct{}
	winner *laneState
}

// Fetch returns the first consumer-validated block. Canceled competing attempts
// retain their capacity until they exit; Close joins that remaining ownership.
func (s *Set) Fetch(
	ctx context.Context,
	request Request,
) (Block, error) {
	demand, validate := request.Demand, request.Validate
	if err := ctx.Err(); err != nil {
		return Block{}, err
	}
	if validate == nil {
		failure := failurestate.DependencyContract(ErrInvalidRequest)
		return Block{}, failurestate.BoundaryError(failure, ErrInvalidRequest)
	}
	if !s.beginFetch() {
		return Block{}, ErrClosed
	}
	defer s.fetches.Done()
	attempted := make(map[Identity]struct{}, MaxDemandAttempts)
	failures := laneFailureSet{}
	var pendingReassignments []laneResult
	supplemented := false
	defer func() {
		s.resolveLaneReassignments(pendingReassignments, false)
	}()
	for len(attempted) < MaxDemandAttempts {
		candidates, exhausted, err := s.candidates(ctx, attempted, request)
		if err != nil {
			normalized := failurestate.Require(normalizeSourceBoundary(ctx, err))
			return Block{}, failurestate.BoundaryError(normalized, err)
		}
		if exhausted {
			return Block{}, failurestate.BoundaryError(failures.failure, failures.diagnostic)
		}
		// Reassignment is an admitted action, not an inference from a retryable
		// error. Candidate selection has already reserved the subsequent round.
		s.resolveLaneReassignments(pendingReassignments, true)
		pendingReassignments = nil
		for _, state := range candidates {
			attempted[state.identity] = struct{}{}
		}
		round := s.runLaneRound(ctx, demand, validate, candidates, attempted, &supplemented, request.Claim)
		switch round.kind {
		case laneRoundSucceeded:
			return round.record, nil
		case laneRoundInterrupted:
			return Block{}, round.err
		case laneRoundFailed:
			var reassignable bool
			failures, reassignable = reduceLaneFailures(failures, round.failures)
			if !reassignable {
				return Block{}, failurestate.BoundaryError(failures.failure, failures.diagnostic)
			}
			pendingReassignments = round.failures
		}
	}
	return Block{}, failurestate.BoundaryError(failures.failure, failures.diagnostic)
}

func (s *Set) beginFetch() bool {
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

func (s *Set) runLaneRound(
	ctx context.Context,
	demand Demand,
	validate func(records.BlockRecord) error,
	candidates []*laneAdmission,
	attempted map[Identity]struct{},
	supplemented *bool,
	claim CapacityClaim,
) laneRoundResult {
	raceContext, cancel := context.WithCancelCause(ctx)
	stopLifecycle := context.AfterFunc(s.lifecycle, func() { cancel(ErrClosed) })
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
	results := make(chan laneResult, MaxDemandAttempts)
	for _, state := range candidates {
		go s.fetchLane(raceContext, demand, validate, state, decision, results, state.purpose)
	}
	started, estimate := candidates[0].started, candidates[0].expected
	active := len(candidates)
	startRescue := func() {
		if *supplemented || len(candidates) != 1 || raceContext.Err() != nil {
			return
		}
		state := s.rescue(demand, attempted, s.now().Sub(started), estimate, claim)
		if state == nil {
			return
		}
		*supplemented = true
		active++
		go s.fetchLane(raceContext, demand, validate, state, decision, results, lanescheduling.Rescue)
	}
	rescue := s.after(rescuePollInterval)
	failures := make([]laneResult, 0, active)
	for active > 0 {
		select {
		case <-raceContext.Done():
			return interruptedLaneRound(ctx)
		case <-rescue:
			startRescue()
			rescue = s.after(rescuePollInterval)
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
		err = ErrClosed
	}
	return laneRoundResult{kind: laneRoundInterrupted, err: err}
}

func (a *laneAdmission) releaseCapacity() {
	if a.release != nil {
		release := a.release
		a.release = nil
		release()
	}
}

func (s *Set) fetchLane(
	ctx context.Context,
	demand Demand,
	validate func(records.BlockRecord) error,
	admission *laneAdmission,
	decision *laneRoundDecision,
	results chan<- laneResult,
	purpose lanescheduling.Purpose,
) {
	defer s.attempts.Done()
	state := admission.laneState
	defer admission.releaseCapacity()
	if purpose != lanescheduling.Content {
		defer func() { s.mu.Lock(); s.exploration.Release(purpose); s.mu.Unlock() }()
	}
	started := admission.started
	bytes := blockDemandBytes(demand)
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		s.mu.Lock()
		queued, rate := state.performance.PendingBytes, state.performance.BytesPerSecond
		s.mu.Unlock()
		slog.DebugContext(ctx, "content lane dispatched",
			"protocol_session_id", s.sessionID, "lane_id", state.identity.ID, "lane_epoch", state.identity.Epoch,
			"file_id", demand.Descriptor.FileID(), "block_index", demand.Index, "route", state.route,
			"purpose", purpose, "expected_ms", admission.expected.Milliseconds(), "pending_bytes", queued, "bytes_per_second", rate)
	}
	var record records.BlockRecord
	fetchErr := ctx.Err()
	if fetchErr == nil {
		record, fetchErr = admission.source.FetchBlock(ctx, demand, func(bytes uint64) {
			s.mu.Lock()
			if admission.permit.Receive(bytes, s.now()) {
				s.notifyAdmissionChangedLocked()
			}
			s.mu.Unlock()
		})
	}
	if fetchErr == nil {
		fetchErr = validate(record)
	}
	notAdmitted := isDemandNotAdmitted(fetchErr)
	normalized := failurestate.Require(normalizeSourceBoundary(ctx, fetchErr))
	canceled := normalized != nil && normalized.Canceled()
	reassignable := !canceled && (notAdmitted || isDemandReassignableAfterRetirement(fetchErr))
	elapsed := s.now().Sub(started)
	s.mu.Lock()
	if fetchErr == nil {
		admission.permit.Complete(s.now())
	}
	admission.permit.Close()
	state.performance.Complete(s.now(), bytes, fetchErr == nil)
	// Completion can change preferred-lane ordering even if receipt capacity
	// was already available; capacity-only edge notifications cannot replace it.
	s.notifyAdmissionChangedLocked()
	s.mu.Unlock()
	if fetchErr != nil {
		// Failed outcomes carry no plaintext. Return their capacity before
		// publication, independently of the round's later settlement bookkeeping.
		record = records.BlockRecord{}
		admission.releaseCapacity()
	}
	results <- laneResult{
		state: state, record: record, err: fetchErr,
		normalized: normalized, notAdmitted: notAdmitted, reassignable: reassignable,
	}
	<-decision.done
	if canceled && decision.winner != nil && decision.winner != state {
		s.mu.Lock()
		state.performance.Superseded(bytes, elapsed)
		s.mu.Unlock()
	}
	s.finish(state, record, fetchErr, canceled, decision.winner == state)
}

func (s *Set) finish(
	state *laneState,
	record records.BlockRecord,
	err error,
	canceled bool,
	selected bool,
) {
	s.mu.Lock()
	if selected {
		state.settlement.addDelivered(uint64(record.DataLength()))
		var incomplete bool
		saturatingLaneCounter(&state.usefulBytes, uint64(record.DataLength()), &incomplete)
		state.lastUsefulAt = s.now()
		s.recordContentActivityLocked(state.route, uint64(record.DataLength()))
	}
	state.inflight--
	if err != nil {
		state.recordFailure(canceled)
	} else {
		state.failures = 0
	}
	settlement := s.settleLaneLocked(state)
	s.mu.Unlock()
	s.publishLaneSettlement(settlement)
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
