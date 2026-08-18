package v2peer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	DefaultPeerNegotiationBudget          = 15 * time.Second
	DefaultPeerAdmissionBudget            = 20 * time.Second
	MinimumLaneGrantCompletionMargin      = 5 * time.Second
	MaximumPeerAdmissionBudget            = protocolsession.LaneGrantTTL - MinimumLaneGrantCompletionMargin
	SenderMaxActivePeerAttemptsPerSession = 1
	SenderMaxSessionEvidenceIdentities    = 64
)

var (
	ErrPeerNegotiationTimeout = errors.New("peer negotiation phase timed out")
	ErrPeerAdmissionTimeout   = errors.New("peer lane-admission phase timed out")
)

type PeerAttemptPhase string

const (
	PeerAttemptPhaseNegotiation PeerAttemptPhase = "negotiation"
	PeerAttemptPhaseAdmission   PeerAttemptPhase = "lane_admission"
)

type PeerPhaseTimer interface {
	C() <-chan time.Time
	Stop()
}

type PeerPhaseTimerSource interface {
	NewPeerPhaseTimer(PeerAttemptPhase, time.Duration) (PeerPhaseTimer, error)
}

type systemPeerPhaseTimer struct{ timer *time.Timer }

func (timer systemPeerPhaseTimer) C() <-chan time.Time { return timer.timer.C }

func (timer systemPeerPhaseTimer) Stop() {
	if timer.timer == nil || timer.timer.Stop() {
		return
	}
	select {
	case <-timer.timer.C:
	default:
	}
}

type systemPeerPhaseTimerSource struct{}

func (systemPeerPhaseTimerSource) NewPeerPhaseTimer(
	_ PeerAttemptPhase,
	duration time.Duration,
) (PeerPhaseTimer, error) {
	return systemPeerPhaseTimer{timer: time.NewTimer(duration)}, nil
}

func validPeerPhaseBudgets(negotiation, admission time.Duration) bool {
	return negotiation > 0 && admission > 0 && admission <= MaximumPeerAdmissionBudget
}

func peerPhaseTimeout(phase PeerAttemptPhase) error {
	switch phase {
	case PeerAttemptPhaseNegotiation:
		return ErrPeerNegotiationTimeout
	case PeerAttemptPhaseAdmission:
		return ErrPeerAdmissionTimeout
	default:
		return ErrConfig
	}
}

type peerPhaseState uint8

const (
	peerPhaseReserved peerPhaseState = iota
	peerPhaseNegotiating
	peerPhaseAdmissionWaiting
	peerPhaseAdmissionSettling
	peerPhaseAdmitted
	peerPhaseTerminal
)

type peerPhaseExpiration struct {
	phase      PeerAttemptPhase
	generation uint64
	cause      error
}

type peerPhaseDeadline struct {
	timer    PeerPhaseTimer
	cancel   context.CancelCauseFunc
	stopped  chan struct{}
	stopOnce sync.Once
}

func (deadline *peerPhaseDeadline) stop(cause error) {
	if deadline == nil {
		return
	}
	deadline.stopOnce.Do(func() {
		close(deadline.stopped)
		deadline.timer.Stop()
		if cause == nil {
			cause = context.Canceled
		}
		deadline.cancel(cause)
	})
}

type peerPhaseLifecycle struct {
	mu                sync.Mutex
	timers            PeerPhaseTimerSource
	negotiationBudget time.Duration
	admissionBudget   time.Duration
	state             peerPhaseState
	generation        uint64
	deadline          *peerPhaseDeadline
	pendingExpiration peerPhaseExpiration
	expirationOwned   bool
	expirations       chan peerPhaseExpiration
}

func newPeerPhaseLifecycle(
	timers PeerPhaseTimerSource,
	negotiationBudget time.Duration,
	admissionBudget time.Duration,
) *peerPhaseLifecycle {
	return &peerPhaseLifecycle{
		timers: timers, negotiationBudget: negotiationBudget, admissionBudget: admissionBudget,
		state: peerPhaseReserved, expirations: make(chan peerPhaseExpiration, 2),
	}
}

func (lifecycle *peerPhaseLifecycle) beginNegotiation(
	parent context.Context,
) (context.Context, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.state != peerPhaseReserved || parent == nil {
		return nil, ErrConfig
	}
	ctx, err := lifecycle.armLocked(parent, PeerAttemptPhaseNegotiation, lifecycle.negotiationBudget)
	if err != nil {
		lifecycle.state = peerPhaseTerminal
		return nil, err
	}
	lifecycle.state = peerPhaseNegotiating
	return ctx, nil
}

func (lifecycle *peerPhaseLifecycle) beginAdmission(
	parent context.Context,
) (context.Context, bool, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.state != peerPhaseNegotiating || parent == nil {
		return nil, false, nil
	}
	lifecycle.stopDeadlineLocked(nil)
	ctx, err := lifecycle.armLocked(parent, PeerAttemptPhaseAdmission, lifecycle.admissionBudget)
	if err != nil {
		lifecycle.state = peerPhaseTerminal
		return nil, false, err
	}
	lifecycle.state = peerPhaseAdmissionWaiting
	return ctx, true, nil
}

func (lifecycle *peerPhaseLifecycle) beginAuthenticatedSettlement() bool {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.state != peerPhaseAdmissionWaiting {
		return false
	}
	lifecycle.state = peerPhaseAdmissionSettling
	return true
}

func (lifecycle *peerPhaseLifecycle) settleSenderAdmission(
	settlementBegan bool,
	admitted bool,
) bool {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	switch {
	case settlementBegan && lifecycle.state != peerPhaseAdmissionSettling:
		return false
	case !settlementBegan && lifecycle.state != peerPhaseAdmissionWaiting:
		return false
	}
	lifecycle.stopDeadlineLocked(nil)
	if admitted {
		lifecycle.state = peerPhaseAdmitted
	} else {
		lifecycle.state = peerPhaseTerminal
	}
	return true
}

func (lifecycle *peerPhaseLifecycle) settleReceiverAdmission(
	settlement receiverAdmissionSettlement,
) bool {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if settlement.authenticated() {
		// A verified WS2B/WS2N proves the response boundary preceded the
		// cancellation observed by AttachLane, even when delivery to this
		// goroutine loses the scheduler race with the timer event.
		if lifecycle.state != peerPhaseAdmissionWaiting &&
			lifecycle.state != peerPhaseTerminal {
			return false
		}
	} else if settlement != receiverAdmissionUnverified ||
		lifecycle.state != peerPhaseAdmissionWaiting {
		return false
	}
	lifecycle.stopDeadlineLocked(nil)
	if settlement == receiverAdmissionInstalled {
		lifecycle.state = peerPhaseAdmitted
	} else {
		lifecycle.state = peerPhaseTerminal
	}
	return true
}

func (lifecycle *peerPhaseLifecycle) expire(
	expiration peerPhaseExpiration,
) (bool, bool) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if expiration.generation != lifecycle.generation ||
		lifecycle.deadline == nil {
		return false, false
	}
	if lifecycle.pendingExpiration.generation == expiration.generation {
		if lifecycle.expirationOwned {
			return true, false
		}
		if lifecycle.state == peerPhaseAdmissionSettling {
			return false, true
		}
	}
	if expiration.phase != PeerAttemptPhaseNegotiation ||
		lifecycle.state != peerPhaseNegotiating {
		return false, false
	}
	lifecycle.state = peerPhaseTerminal
	lifecycle.expirationOwned = true
	return true, false
}

func (lifecycle *peerPhaseLifecycle) deadlineFired(expiration peerPhaseExpiration) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if expiration.generation != lifecycle.generation || lifecycle.deadline == nil {
		return
	}
	lifecycle.pendingExpiration = expiration
	lifecycle.expirationOwned = false
	if expiration.phase != PeerAttemptPhaseAdmission {
		return
	}
	switch lifecycle.state {
	case peerPhaseAdmissionWaiting:
		lifecycle.state = peerPhaseTerminal
		lifecycle.expirationOwned = true
	case peerPhaseAdmissionSettling:
		// The timer may cancel I/O, but authenticated settlement retains the
		// public outcome once its synchronous gate has won.
	default:
		lifecycle.pendingExpiration = peerPhaseExpiration{}
	}
}

func (lifecycle *peerPhaseLifecycle) terminate(cause error) bool {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.state == peerPhaseAdmissionSettling {
		if lifecycle.deadline != nil {
			lifecycle.deadline.stop(cause)
		}
		return false
	}
	if lifecycle.state == peerPhaseTerminal {
		return true
	}
	if lifecycle.state != peerPhaseAdmitted {
		lifecycle.state = peerPhaseTerminal
	}
	lifecycle.stopDeadlineLocked(cause)
	return true
}

func (lifecycle *peerPhaseLifecycle) admitted() bool {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	return lifecycle.state == peerPhaseAdmitted
}

func (lifecycle *peerPhaseLifecycle) expirationEvents() <-chan peerPhaseExpiration {
	if lifecycle == nil {
		return nil
	}
	return lifecycle.expirations
}

func (lifecycle *peerPhaseLifecycle) armLocked(
	parent context.Context,
	phase PeerAttemptPhase,
	budget time.Duration,
) (context.Context, error) {
	if lifecycle.timers == nil || !validPeerPhaseBudgets(
		lifecycle.negotiationBudget,
		lifecycle.admissionBudget,
	) {
		return nil, ErrConfig
	}
	timer, err := lifecycle.timers.NewPeerPhaseTimer(phase, budget)
	if err != nil || timer == nil {
		return nil, errors.Join(ErrConfig, err)
	}
	ctx, cancel := context.WithCancelCause(parent)
	lifecycle.generation++
	lifecycle.pendingExpiration = peerPhaseExpiration{}
	lifecycle.expirationOwned = false
	expiration := peerPhaseExpiration{
		phase: phase, generation: lifecycle.generation, cause: peerPhaseTimeout(phase),
	}
	deadline := &peerPhaseDeadline{
		timer: timer, cancel: cancel, stopped: make(chan struct{}),
	}
	lifecycle.deadline = deadline
	go func() {
		select {
		case <-timer.C():
			lifecycle.deadlineFired(expiration)
			cancel(expiration.cause)
			select {
			case lifecycle.expirations <- expiration:
			case <-deadline.stopped:
			}
		case <-deadline.stopped:
		}
	}()
	return ctx, nil
}

func (lifecycle *peerPhaseLifecycle) stopDeadlineLocked(cause error) {
	if lifecycle.deadline == nil {
		return
	}
	lifecycle.deadline.stop(cause)
	lifecycle.deadline = nil
}

type ownedPeerDataChannel struct {
	PeerDataChannel
	peer peerCloseOwner

	mu       sync.Mutex
	consumed bool
	once     sync.Once
	teardown peerTransportTeardown
}

func newOwnedPeerDataChannel(peer peerCloseOwner, channel PeerDataChannel) *ownedPeerDataChannel {
	return &ownedPeerDataChannel{PeerDataChannel: channel, peer: peer}
}

func (owner *ownedPeerDataChannel) consume() bool {
	if owner == nil {
		return false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.consumed {
		return false
	}
	owner.consumed = true
	return true
}

func (owner *ownedPeerDataChannel) closeIfUnconsumed() peerTransportTeardown {
	if owner == nil {
		return peerTransportTeardown{}
	}
	owner.mu.Lock()
	if owner.consumed {
		teardown := owner.teardown
		owner.mu.Unlock()
		return teardown
	}
	owner.consumed = true
	owner.mu.Unlock()
	_ = owner.Close()
	return owner.teardownSnapshot()
}

func (owner *ownedPeerDataChannel) Close() error {
	if owner == nil {
		return nil
	}
	owner.once.Do(func() {
		teardown := teardownPeerTransport(owner.peer, owner.PeerDataChannel)
		owner.mu.Lock()
		owner.teardown = teardown
		owner.mu.Unlock()
	})
	return owner.teardownSnapshot().cause()
}

func (owner *ownedPeerDataChannel) teardownSnapshot() peerTransportTeardown {
	if owner == nil {
		return peerTransportTeardown{}
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return peerTransportTeardown{
		transitions:       append([]PeerTeardownTransition(nil), owner.teardown.transitions...),
		peerShutdownError: owner.teardown.peerShutdownError,
		channelDrainError: owner.teardown.channelDrainError,
	}
}
