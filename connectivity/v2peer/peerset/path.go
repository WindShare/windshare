package peerset

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type recoveryWave struct {
	started       time.Time
	attempts      int
	opportunities int
}

const MappingOpportunityDelay = 10 * time.Second

type attemptProgress uint8

const (
	attemptUnstarted attemptProgress = iota
	attemptStarted
	attemptAdmitted
)

type attemptOpportunity struct {
	binding v2signal.Binding
	start   Starter
	close   func()
	budget  time.Duration
	refund  func(time.Duration)
}

func (p *Path) run() {
	defer close(p.done)
	defer p.supply.Close()
	maintenance, cancel := context.WithCancel(p.ctx)
	done := make(chan struct{})
	go p.maintainResources(maintenance, done)
	defer func() { cancel(); <-done; p.closeResources() }()
	wave := recoveryWave{started: p.owner.config.Clock.Now()}
	for {
		if stop := p.stopReason(); stop != nil {
			p.finish(*stop)
			return
		}
		if wave.exhausted(p.owner.config.Clock.Now(), p.minimumOpportunity()) {
			if stop := p.nextWave(&wave); stop != nil {
				p.finish(*stop)
				return
			}
		}
		opportunity, stop := p.prepareOpportunity(&wave)
		if stop != nil {
			p.finish(*stop)
			return
		}
		if opportunity == nil {
			continue
		}
		wave.attempts++
		wave.opportunities++
		p.setResourceActive(true)
		result, progress := p.executePrepared(opportunity)
		if stop := p.afterAttempt(&wave, result, progress); stop != nil {
			p.finish(*stop)
			return
		}
	}
}
func (p *Path) closeResources() {
	p.deferred.settle(false, 0)
	revoke, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 2*time.Second)
	p.sendControl(revoke, protocolsession.PeerPathRevoke)
	cancel()
	if p.config.Native != nil {
		p.config.Native.ClosePath([16]byte(p.key.session), p.key.path)
	}
	p.owner.mu.Lock()
	delete(p.owner.paths, p.key)
	p.slot.Close()
	p.owner.mu.Unlock()
}
func (p *Path) stopReason() *Result {
	p.mu.Lock()
	retired := p.retired
	p.mu.Unlock()
	if p.config.Native != nil && p.config.Native.Retired([16]byte(p.key.session), p.key.path) {
		retired = true
	}
	if retired {
		return &Result{Scope: protocolsession.PeerFailurePathTerminal, Cause: ErrPathTerminal}
	}
	if p.ctx.Err() != nil || p.currentDemand() == NoDemand {
		return &Result{Stopped: true}
	}
	return nil
}
func (w recoveryWave) remaining(now time.Time) time.Duration {
	return WaveBudget - now.Sub(w.started)
}

func (w recoveryWave) exhausted(now time.Time, minimum time.Duration) bool {
	return w.attempts >= AttemptsPerWave || ((w.opportunities > 0 || w.attempts > 0) && w.remaining(now) < minimum)
}
func (p *Path) nextWave(wave *recoveryWave) *Result {
	p.deferred.settle(false, 0)
	p.deferred = nil
	p.setResourceActive(false)
	p.mu.Lock()
	if p.config.StopAfterWave || p.demand == BrowseDemand {
		result := Result{Cause: ErrWaveExhausted}
		p.finishLocked(result)
		p.mu.Unlock()
		return &result
	}
	p.mu.Unlock()
	if !p.wait(max(RefillWindow-p.owner.config.Clock.Now().Sub(wave.started), 0)) {
		return &Result{Stopped: true}
	}
	*wave = recoveryWave{started: p.owner.config.Clock.Now()}
	return nil
}
func (p *Path) awaitMappingOpportunity(wave recoveryWave) bool {
	now := p.owner.config.Clock.Now()
	if p.config.Purpose != v2signal.ThroughputExploration && wave.attempts == 0 && p.deferred == nil && p.config.Native != nil && p.currentDemand() == ContentDemand {
		p.deferred = p.budget().reserveDeferred(now, p.attemptBudget())
	}
	if p.deferred == nil || wave.attempts != AttemptsPerWave-1 || p.mappingReady() {
		return true
	}
	delay := max(min(MappingOpportunityDelay, WaveBudget-p.minimumOpportunity())-now.Sub(wave.started), 0)
	p.setResourceActive(true)
	return p.wait(delay)
}
func (p *Path) prepareOpportunity(wave *recoveryWave) (*attemptOpportunity, *Result) {
	if !p.awaitMappingOpportunity(*wave) {
		return nil, &Result{Stopped: true}
	}
	now := p.owner.config.Clock.Now()
	if wave.opportunities == 0 {
		wave.started = now
	} else if wave.exhausted(now, p.minimumOpportunity()) {
		return nil, nil
	}
	limit := min(p.attemptBudget(), wave.remaining(now))
	refund, delay := p.reserveAttemptBudget(now, wave.attempts, limit)
	if refund == nil {
		if p.config.StopAfterWave {
			return nil, &Result{Cause: ErrWaveExhausted}
		}
		if !p.wait(delay) {
			return nil, &Result{Stopped: true}
		}
		return nil, nil
	}
	if wave.opportunities == 0 && p.config.Native != nil {
		p.config.Native.BeginWave([16]byte(p.key.session), p.key.path)
	}
	binding, err := p.freshBinding()
	if err != nil {
		refund(0)
		p.budget().refundUnstartedAttempt()
		return nil, &Result{Scope: protocolsession.PeerFailurePathTerminal, Cause: err}
	}
	opportunity, err := p.prepareProvider(wave, binding, limit, refund)
	if err == nil {
		return opportunity, nil
	}
	refund(0)
	wave.attempts++
	wave.opportunities++
	return nil, p.afterAttempt(wave, Result{Cause: err, Stopped: p.ctx.Err() != nil}, attemptUnstarted)
}

func (p *Path) reserveAttemptBudget(now time.Time, attempts int, limit time.Duration) (func(time.Duration), time.Duration) {
	ready := p.consumeMapping()
	if p.deferred != nil && (ready || attempts == AttemptsPerWave-1) {
		return p.takeDeferred(), 0
	}
	refund, delay := p.budget().reserve(now, limit)
	if refund == nil && p.deferred != nil {
		return p.takeDeferred(), 0
	}
	return refund, delay
}
func (p *Path) takeDeferred() func(time.Duration) {
	reserved := p.deferred
	p.deferred = nil
	return func(used time.Duration) { reserved.settle(true, used) }
}
func (p *Path) freshBinding() (v2signal.Binding, error) {
	ordinal, err := p.slot.Next()
	if err != nil {
		return v2signal.Binding{}, err
	}
	purpose := p.config.Purpose
	if purpose == v2signal.InitialConnectivity && ordinal.Sequence > 1 {
		purpose = v2signal.Recovery
	}
	binding := v2signal.Binding{Purpose: purpose, PeerSlot: ordinal.Slot, PeerPathID: p.key.path, AttemptSequence: ordinal.Sequence}
	p.owner.mu.Lock()
	_, err = io.ReadFull(p.owner.config.Random, binding.AttemptID[:])
	p.owner.mu.Unlock()
	if err != nil || binding.Validate() != nil {
		return binding, errors.Join(ErrConfig, err)
	}
	return binding, nil
}
func (p *Path) afterAttempt(wave *recoveryWave, result Result, progress attemptProgress) *Result {
	deferred := result.Scope == protocolsession.PeerFailureResourceDeferred || errors.Is(result.Cause, socketauthority.ErrCapacity) ||
		(progress == attemptUnstarted && errors.Is(result.Cause, context.DeadlineExceeded))
	if progress == attemptUnstarted || (deferred && progress == attemptStarted) {
		p.budget().refundUnstartedAttempt()
	}
	p.setResourceActive(p.deferred != nil)
	if p.config.Purpose == v2signal.ThroughputExploration {
		return &result
	}
	if p.currentDemand() == BrowseDemand && p.ctx.Err() == nil && result.Scope != protocolsession.PeerFailureSessionTerminal && result.Scope != protocolsession.PeerFailurePathTerminal {
		if stop := p.awaitContentDemand(); stop != nil {
			return stop
		}
		*wave = recoveryWave{started: p.owner.config.Clock.Now()}
		return nil
	}
	if result.Stopped || result.Scope == protocolsession.PeerFailureSessionTerminal || result.Scope == protocolsession.PeerFailurePathTerminal {
		return &result
	}
	if progress == attemptAdmitted {
		*wave = recoveryWave{started: p.owner.config.Clock.Now()}
		return nil
	}
	if errors.Is(result.Cause, ErrWaveExhausted) {
		return p.nextWave(wave)
	}
	// Capacity deferral proves the remote did not start ICE. Preserve the finite
	// wall-clock wave and application rate limits, but return its negotiation allowance.
	if deferred {
		wave.attempts--
		if !p.wait(ResourceRetryDelay) {
			return &Result{Stopped: true}
		}
		return nil
	}
	delay := time.Duration(wave.attempts) * time.Second
	if !p.wait(delay) {
		return &Result{Stopped: true}
	}
	return nil
}

// Finished prewarm work stays dormant until the same intent acquires content
// demand. This preserves the one speculative attempt without making a slow
// catalog browse permanently disable later direct recovery.
func (p *Path) awaitContentDemand() *Result {
	for {
		p.mu.Lock()
		if p.retired {
			result := Result{Scope: protocolsession.PeerFailurePathTerminal, Cause: ErrPathTerminal}
			p.finishLocked(result)
			p.mu.Unlock()
			return &result
		}
		if p.demand != BrowseDemand {
			if p.ctx.Err() != nil || p.demand != ContentDemand {
				result := Result{Stopped: true}
				p.finishLocked(result)
				p.mu.Unlock()
				return &result
			}
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		select {
		case <-p.ctx.Done():
			result := Result{Stopped: true}
			p.finish(result)
			return &result
		case <-p.wake:
		}
	}
}

func (p *Path) executeStart(start Starter, binding v2signal.Binding, opportunity time.Duration, refund func(time.Duration)) (Result, attemptProgress) {
	started := p.owner.config.Clock.Now()
	child, cancel := context.WithCancel(p.ctx)
	defer cancel()
	timer := p.owner.config.Clock.NewTimer(opportunity)
	defer timer.Stop()
	charged := false
	settle := func() {
		if !charged {
			charged = true
			refund(p.owner.config.Clock.Now().Sub(started))
		}
	}
	defer settle()
	deadlineExpired := make(chan time.Time)
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		select {
		case <-timer.C():
			cancel()
			close(deadlineExpired)
		case <-child.Done():
		}
	}()
	defer func() { cancel(); <-watchdogDone }()
	attempt, err := start(child, binding)
	if child.Err() != nil {
		p.closeAttempt(attempt)
		return Result{Cause: context.DeadlineExceeded, Stopped: p.ctx.Err() != nil}, attemptStarted
	}
	if err != nil || attempt == nil {
		return Result{Cause: err}, attemptStarted
	}
	p.mu.Lock()
	p.current = attempt
	p.binding = binding
	p.mu.Unlock()
	p.resourceDemand(false)
	defer func() {
		p.mu.Lock()
		// Revoke recovery authority before joining the old provider. A late
		// observation during Close must not restart the next generation.
		p.current = nil
		p.binding = v2signal.Binding{}
		p.lane = session.LaneIdentity{}
		p.restartCause = nil
		p.mu.Unlock()
		p.closeAttempt(attempt)
	}()
	ready := attempt.Ready()
	deadline := (<-chan time.Time)(deadlineExpired)
	var retention Timer
	var retainUntil <-chan time.Time
	defer func() {
		if retention != nil {
			retention.Stop()
		}
	}()
	progress := attemptStarted
	for {
		select {
		case <-p.ctx.Done():
			return Result{Stopped: true}, progress
		case <-p.wake:
			if cause := p.consumeRestart(); cause != nil {
				return Result{Cause: cause}, progress
			}
			switch p.currentDemand() {
			case NoDemand:
				return Result{Stopped: true}, progress
			case ContentDemand:
				if retention != nil {
					retention.Stop()
					retainUntil = nil
				}
			}
		case <-deadline:
			// The enclosing attempt cap never resets when the provider advances stages.
			cancel()
			return Result{Cause: context.DeadlineExceeded}, progress
		case <-retainUntil:
			if p.currentDemand() != ContentDemand {
				return Result{}, progress
			}
			retainUntil = nil
		case <-ready:
			ready = nil
			progress = attemptAdmitted
			p.deferred.settle(false, 0)
			p.deferred = nil
			deadline = nil
			timer.Stop()
			settle()
			p.mu.Lock()
			p.lane, _ = attempt.Lane()
			p.mu.Unlock()
			p.readyOnce.Do(func() { close(p.ready) })
			p.resourceDemand(true)
			if p.currentDemand() == BrowseDemand {
				retention = p.owner.config.Clock.NewTimer(PrewarmRetention)
				retainUntil = retention.C()
			}
		case <-attempt.Done():
			outcome := attempt.Outcome()
			return Result{Scope: outcome.RecoveryScope(), Cause: outcome.RetainedCause(), Stopped: outcome.LocallyCanceled() && p.ctx.Err() != nil}, progress
		}
	}
}

func (p *Path) closeAttempt(attempt Attempt) {
	if attempt == nil {
		return
	}
	initial, admitted := attempt.Lane()
	if admitted && p.config.Content != nil {
		p.config.Content.DetachLane(initial)
	}
	_ = attempt.Close()
	// Close joins attachment as well as negotiation. Admission can win the
	// cancellation race after the first snapshot, transferring a final lane.
	if final, attached := attempt.Lane(); attached && (!admitted || final != initial) && p.config.Content != nil {
		p.config.Content.DetachLane(final)
	}
}
