package lanes

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

const priorityAgingWindow = time.Second

type pendingAdmission struct {
	ctx             context.Context
	request         Request
	attempted       map[Identity]struct{}
	queuedAt        time.Time
	sequence        uint64
	order           WorkOrder
	selected        []*laneAdmission
	exhausted       bool
	err             error
	done            bool
	capacityChanged <-chan struct{}
	probeAt         time.Time
	blocked         CapacityBlock
}

func (s *Set) candidates(ctx context.Context, attempted map[Identity]struct{}, request Request) ([]*laneAdmission, bool, error) {
	s.mu.Lock()
	s.allocationSequence++
	pending := &pendingAdmission{ctx: ctx, request: request, attempted: attempted, queuedAt: s.now(), sequence: s.allocationSequence}
	s.pending = append(s.pending, pending)
	defer func() {
		s.mu.Lock()
		s.pending = slices.DeleteFunc(s.pending, func(item *pendingAdmission) bool { return item == pending })
		s.mu.Unlock()
	}()
	for {
		s.allocatePendingLocked()
		if pending.done {
			s.mu.Unlock()
			return pending.selected, pending.exhausted, pending.err
		}
		changed, capacityChanged, probeAt := s.admissionChanged, pending.capacityChanged, pending.probeAt
		var probe <-chan time.Time
		if !probeAt.IsZero() {
			probe = s.after(max(0, probeAt.Sub(s.now())))
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-s.lifecycle.Done():
		case <-changed:
		case <-capacityChanged:
		case <-probe:
		}
		s.mu.Lock()
	}
}

// Every waiter participates in the same allocation pass. Receipt, capacity and
// route changes re-evaluate queued work before any lane can acquire a backlog.
func (s *Set) allocatePendingLocked() {
	now := s.now()
	for _, pending := range s.pending {
		if pending.done {
			continue
		}
		switch {
		case s.closed || s.lifecycle.Err() != nil:
			pending.err, pending.done = ErrClosed, true
		case pending.ctx.Err() != nil:
			pending.err, pending.done = pending.ctx.Err(), true
		default:
			if pending.request.Order != nil {
				pending.order = pending.request.Order()
			}
		}
	}
	slices.SortStableFunc(s.pending, func(left, right *pendingAdmission) int { return comparePending(left, right, now) })
	changed := false
	for _, pending := range s.pending {
		if pending.done {
			continue
		}
		s.allocateOneLocked(pending, now)
		changed = changed || pending.done
	}
	if changed {
		s.notifyAdmissionChangedLocked()
	}
}

func comparePending(left, right *pendingAdmission, now time.Time) int {
	leftAged, rightAged := now.Sub(left.queuedAt) >= priorityAgingWindow, now.Sub(right.queuedAt) >= priorityAgingWindow
	if leftAged != rightAged {
		if leftAged {
			return -1
		}
		return 1
	}
	if !leftAged {
		if order := cmp.Compare(priorityRank(left.order.Priority), priorityRank(right.order.Priority)); order != 0 {
			return order
		}
	}
	if order := cmp.Compare(left.order.Distance, right.order.Distance); order != 0 {
		return order
	}
	if order := cmp.Compare(left.order.LastServed, right.order.LastServed); order != 0 {
		return order
	}
	return cmp.Compare(left.sequence, right.sequence)
}

func (s *Set) allocateOneLocked(pending *pendingAdmission, now time.Time) {
	remaining := MaxDemandAttempts - len(pending.attempted)
	ordered, suspended := s.eligiblePendingLanesLocked(pending)
	if remaining <= 0 || (len(ordered) == 0 && !suspended) {
		pending.exhausted, pending.done = true, true
		return
	}
	pending.blocked, pending.probeAt, pending.capacityChanged = CapacityNetwork, time.Time{}, nil
	if pending.request.Ready != nil {
		readiness := pending.request.Ready()
		if readiness.Kind != CapacityReady {
			pending.blocked, pending.capacityChanged = readiness.Kind, readiness.Changed
			return
		}
	}
	if len(ordered) == 0 {
		pending.blocked = CapacityRoute
		return
	}
	s.orderLanesLocked(ordered, blockDemandBytes(pending.request.Demand))
	candidate, purpose := s.pendingCandidateLocked(pending, ordered, now)
	if candidate == nil {
		return
	}
	admission, wake, err := s.admitLaneLocked(candidate, pending.request.Demand, pending.request.Claim)
	pending.capacityChanged = wake
	if err != nil {
		pending.err, pending.done = err, true
		return
	}
	if admission == nil {
		pending.blocked = CapacityConsumer
		return
	}
	admission.purpose = purpose
	candidate.sampling.Offered(blockDemandBytes(pending.request.Demand), now)
	if purpose == lanescheduling.Probe || purpose == lanescheduling.Sample {
		s.exploration.Acquire(purpose, now)
	}
	s.attempts.Add(1)
	pending.selected, pending.done = []*laneAdmission{admission}, true
}

func priorityRank(priority WorkPriority) int {
	switch priority {
	case Preview:
		return 0
	case Prefetch:
		return 2
	default:
		return 1
	}
}

func (s *Set) eligiblePendingLanesLocked(pending *pendingAdmission) ([]*laneState, bool) {
	ordered := make([]*laneState, 0, len(s.lanes))
	suspended := len(s.routeSupplies) != 0
	for laneID, hold := range s.contentSuspensions {
		if s.lanes[laneID] == nil && s.contentRoutePolicy.Allows(hold.route) {
			suspended = true
		}
	}
	for _, state := range s.lanes {
		if _, tried := pending.attempted[state.identity]; tried {
			continue
		}
		// Policy exclusion is immutable for this session and cannot promise
		// future availability. Only an explicit hold or route owner can do that.
		if !s.contentRoutePolicy.Allows(state.route) {
			continue
		}
		if s.contentSuspensions[state.identity.ID] != nil {
			suspended = true
			continue
		}
		ordered = append(ordered, state)
	}
	return ordered, suspended
}

func (s *Set) orderLanesLocked(ordered []*laneState, bytes uint64) {
	slices.SortFunc(ordered, func(left, right *laneState) int {
		if order := cmp.Compare(left.failures, right.failures); order != 0 {
			return order
		}
		if order := cmp.Compare(laneCompletionCost(left, bytes), laneCompletionCost(right, bytes)); order != 0 {
			return order
		}
		if order := cmp.Compare(left.inflight, right.inflight); order != 0 {
			return order
		}
		return cmp.Compare(left.identity.ID, right.identity.ID)
	})
}

func (s *Set) pendingCandidateLocked(pending *pendingAdmission, ordered []*laneState, now time.Time) (*laneState, lanescheduling.Purpose) {
	primary := ordered[0]
	var active uint32
	for _, state := range s.lanes {
		active += state.inflight
	}
	// Exploration owns a distinct queued block. Protect a lone interactive
	// frontier, while allowing independent readers and bounded read-ahead to learn.
	if primary.performance.HasSuccessfulSample && active > 0 &&
		(pending.order.Distance > 0 || (pending.order.Priority != Preview && active > 1)) {
		for _, candidate := range ordered[1:] {
			if candidate.failures == 0 && candidate.receive.Available() && candidate.performance.PendingBytes == 0 &&
				candidate.sampling.Available(blockDemandBytes(pending.request.Demand), now) && s.exploration.Available(lanescheduling.Sample, now) {
				return candidate, lanescheduling.Sample
			}
		}
		probe := s.probeCandidateLocked(pending, ordered[1:], now)
		if probe != nil && s.exploration.Available(lanescheduling.Probe, now) {
			return probe, lanescheduling.Probe
		}
	}
	if primary.receive.Available() {
		return primary, lanescheduling.Content
	}
	return nil, lanescheduling.Content
}

func (s *Set) probeCandidateLocked(pending *pendingAdmission, ordered []*laneState, now time.Time) *laneState {
	var probe *laneState
	for _, other := range ordered {
		if other.failures != 0 || !other.receive.Available() || other.performance.PendingBytes != 0 {
			continue
		}
		due := maxTime(s.exploration.NextProbe(), other.performance.LastAttempt.Add(lanescheduling.ProbeInterval))
		if now.Before(due) {
			if pending.probeAt.IsZero() || due.Before(pending.probeAt) {
				pending.probeAt = due
			}
			continue
		}
		if probe == nil || other.performance.LastAttempt.Before(probe.performance.LastAttempt) {
			probe = other
		}
	}
	return probe
}

func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

// CapacityBlock distinguishes waiting for transport supply from local holdings.
// Connectivity must not grow paths to solve consumer or memory backpressure.
type CapacityBlock uint8

const (
	CapacityReady CapacityBlock = iota
	CapacityNetwork
	CapacityRoute
	CapacityConsumer
)

type CapacityReadiness struct {
	Kind    CapacityBlock
	Changed <-chan struct{}
}

type SchedulingDemand struct {
	QueuedBlocks         uint64
	QueuedBytes          uint64
	NetworkWaitingBytes  uint64
	CapacityWaitingBytes uint64
	InFlightBytes        uint64
	UsefulBytes          uint64
	OutputBytes          uint64
	UnfinishedBytes      uint64
	RemainingBytes       uint64
	RemainingKnown       bool
}

type RemainingWork struct {
	Bytes uint64
	Known bool
}

type remainingObserver struct{ snapshot func() RemainingWork }

// ObserveRemainingWork follows a job, rather than mistaking its bounded range
// window for the complete selection. Discovery can remain open indefinitely.
func (s *Set) ObserveRemainingWork(snapshot func() RemainingWork) func() {
	observer := &remainingObserver{snapshot: snapshot}
	s.mu.Lock()
	if s.remaining == nil {
		s.remaining = make(map[*remainingObserver]struct{})
	}
	s.remaining[observer] = struct{}{}
	s.mu.Unlock()
	return func() { s.mu.Lock(); delete(s.remaining, observer); s.mu.Unlock() }
}

func (s *Set) SchedulingDemand() SchedulingDemand {
	s.mu.Lock()
	var result SchedulingDemand
	result.OutputBytes = s.outputBytes
	for _, pending := range s.pending {
		if pending.done || pending.ctx.Err() != nil {
			continue
		}
		bytes := blockDemandBytes(pending.request.Demand)
		result.QueuedBlocks++
		result.QueuedBytes += bytes
		if pending.blocked == CapacityNetwork {
			result.NetworkWaitingBytes += bytes
		}
		if pending.blocked == CapacityConsumer {
			result.CapacityWaitingBytes += bytes
		}
	}
	for _, state := range s.lanes {
		result.InFlightBytes += state.performance.PendingBytes
		result.UnfinishedBytes += state.receive.UnfinishedBytes()
	}
	for _, activity := range s.contentActivity {
		result.UsefulBytes += activity.UsefulBytes
	}
	result.RemainingBytes = result.QueuedBytes + result.InFlightBytes
	observers := make([]*remainingObserver, 0, len(s.remaining))
	for observer := range s.remaining {
		observers = append(observers, observer)
	}
	s.mu.Unlock()
	if len(observers) > 0 {
		result.RemainingBytes, result.RemainingKnown = 0, true
		for _, observer := range observers {
			work := observer.snapshot()
			result.RemainingKnown = result.RemainingKnown && work.Known
			if ^uint64(0)-result.RemainingBytes < work.Bytes {
				result.RemainingBytes, result.RemainingKnown = ^uint64(0), false
			} else {
				result.RemainingBytes += work.Bytes
			}
		}
	}
	return result
}

// Output advances after the materializer accepts a range, independently of
// receipt, authentication, and crash-durable checkpoint progress.
func (s *Set) RecordOutput(bytes uint64) {
	s.mu.Lock()
	s.outputBytes += bytes
	s.mu.Unlock()
}
