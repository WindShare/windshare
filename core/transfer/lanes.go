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
	var capability *demandNotAdmittedError
	return errors.As(err, &capability)
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
	ProtocolSessionID protocolsession.ProtocolSessionID
	RaceWidth         int
	Now               func() time.Time
}

type laneState struct {
	identity LaneIdentity
	lane     BlockLane
	inflight uint32
	failures uint32
	latency  time.Duration
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
	sessionID protocolsession.ProtocolSessionID
	raceWidth int
	now       func() time.Time

	lifecycle context.Context
	stop      context.CancelFunc

	mu                  sync.Mutex
	attempts            sync.WaitGroup
	closed              bool
	lanes               map[uint32]*laneState
	contentSuspensions  map[uint32]*contentLaneSuspensionPolicy
	cursor              uint64
	availabilityChanged chan struct{}
}

func NewLaneSet(config LaneSetConfig) (*LaneSet, error) {
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
	lifecycle, stop := context.WithCancel(context.Background())
	return &LaneSet{
		sessionID: config.ProtocolSessionID, raceWidth: config.RaceWidth, now: config.Now,
		lifecycle: lifecycle, stop: stop, lanes: make(map[uint32]*laneState),
		contentSuspensions:  make(map[uint32]*contentLaneSuspensionPolicy),
		availabilityChanged: make(chan struct{}),
	}, nil
}

func (s *LaneSet) Add(identity LaneIdentity, lane BlockLane) error {
	if identity.ID == 0 || lane == nil {
		return ErrInvalidLane
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrLaneClosed
	}
	if current := s.lanes[identity.ID]; current != nil {
		if identity.Epoch <= current.identity.Epoch {
			return ErrStaleLane
		}
		s.lanes[identity.ID] = &laneState{identity: identity, lane: lane}
		s.notifyAvailabilityLocked()
		return nil
	}
	if _, reattachingHeldLane := s.contentSuspensions[identity.ID]; !reattachingHeldLane && s.logicalLaneCountLocked() == MaxLogicalLanes {
		return ErrLaneBudget
	}
	s.lanes[identity.ID] = &laneState{identity: identity, lane: lane}
	s.notifyAvailabilityLocked()
	return nil
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
	defer s.mu.Unlock()
	current := s.lanes[identity.ID]
	if current == nil || current.identity != identity {
		return false
	}
	delete(s.lanes, identity.ID)
	s.notifyAvailabilityLocked()
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
			if _, suspended := s.contentSuspensions[state.identity.ID]; suspended {
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
	close(s.availabilityChanged)
	s.availabilityChanged = make(chan struct{})
}

type laneResult struct {
	state  *laneState
	record records.BlockRecord
	err    error
}

func (s *LaneSet) fetch(
	ctx context.Context,
	demand BlockDemand,
	validate func(records.BlockRecord) error,
) (records.BlockRecord, error) {
	if err := ctx.Err(); err != nil {
		return records.BlockRecord{}, err
	}
	attempted := make(map[LaneIdentity]struct{}, MaxDemandLaneAttempts)
	var combined error
	for len(attempted) < MaxDemandLaneAttempts {
		candidates, exhausted, err := s.candidates(ctx, attempted)
		if err != nil {
			return records.BlockRecord{}, err
		}
		if exhausted {
			return records.BlockRecord{}, combined
		}
		for _, state := range candidates {
			attempted[state.identity] = struct{}{}
		}
		raceContext, cancel := context.WithCancel(ctx)
		stopLifecycle := context.AfterFunc(s.lifecycle, cancel)
		results := make(chan laneResult, len(candidates))
		for _, state := range candidates {
			go func() {
				defer s.attempts.Done()
				started := s.now()
				record, fetchErr := state.lane.FetchBlock(raceContext, demand)
				if fetchErr == nil {
					fetchErr = validate(record)
				}
				s.finish(state, s.now().Sub(started), fetchErr)
				results <- laneResult{state: state, record: record, err: fetchErr}
			}()
		}
		reassignable := true
		for range candidates {
			select {
			case <-raceContext.Done():
				stopLifecycle()
				cancel()
				if ctx.Err() != nil {
					return records.BlockRecord{}, ctx.Err()
				}
				return records.BlockRecord{}, ErrLaneClosed
			case result := <-results:
				if result.err == nil {
					stopLifecycle()
					cancel()
					return result.record, nil
				}
				reassignable = reassignable && isDemandNotAdmitted(result.err)
				combined = errors.Join(combined, fmt.Errorf("lane %d/%d: %w", result.state.identity.ID, result.state.identity.Epoch, result.err))
			}
		}
		stopLifecycle()
		cancel()
		if !reassignable {
			return records.BlockRecord{}, combined
		}
	}
	return records.BlockRecord{}, combined
}

func (s *LaneSet) finish(state *laneState, elapsed time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state.inflight--
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && state.failures < maximumLaneFailures {
			state.failures++
		}
		return
	}
	state.failures = 0
	if elapsed < 0 {
		elapsed = 0
	}
	if state.latency == 0 {
		state.latency = elapsed
	} else {
		state.latency = (state.latency*3 + elapsed) / 4
	}
}

// Stop closes admission and cancels attempts without waiting. BlockLane
// callbacks use it when a lane-local failure must terminate the whole set.
func (s *LaneSet) Stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		clear(s.lanes)
		clear(s.contentSuspensions)
		s.notifyAvailabilityLocked()
	}
	s.mu.Unlock()
	s.stop()
}

// Close is the external ownership boundary. Attempt callbacks call Stop because
// synchronously joining their own attempt would prevent its deferred completion.
func (s *LaneSet) Close() {
	s.Stop()
	// A winner may return without waiting for its hedges, but lane ownership
	// cannot end until cancellation has brought every admitted attempt home.
	s.attempts.Wait()
}
