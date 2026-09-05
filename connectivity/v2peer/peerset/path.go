package peerset

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type recoveryWave struct {
	started  time.Time
	attempts int
}

const MappingOpportunityDelay = 10 * time.Second

type attemptOpportunity struct {
	binding v2signal.Binding
	start   Starter
	close   func()
	budget  time.Duration
	refund  func(time.Duration)
	release func()
}

func (p *Path) run() {
	defer close(p.done)
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
		if wave.exhausted(p.owner.config.Clock.Now()) {
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
		p.setResourceActive(true)
		result, admitted := p.executePrepared(opportunity)
		if stop := p.afterAttempt(&wave, result, admitted); stop != nil {
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
func (w recoveryWave) exhausted(now time.Time) bool {
	return w.attempts >= AttemptsPerWave || (w.attempts > 0 && now.Sub(w.started)+MinimumAttemptOpportunity > WaveBudget)
}
func (p *Path) nextWave(wave *recoveryWave) *Result {
	p.deferred.settle(false, 0)
	p.deferred = nil
	p.setResourceActive(false)
	if p.config.StopAfterWave || p.currentDemand() == BrowseDemand {
		return &Result{Cause: ErrWaveExhausted}
	}
	if !p.wait(max(RefillWindow-p.owner.config.Clock.Now().Sub(wave.started), 0)) {
		return &Result{Stopped: true}
	}
	*wave = recoveryWave{started: p.owner.config.Clock.Now()}
	return nil
}
func (p *Path) awaitMappingOpportunity(wave recoveryWave) bool {
	now := p.owner.config.Clock.Now()
	if wave.attempts == 0 && p.deferred == nil && p.config.Native != nil && p.currentDemand() == ContentDemand {
		p.deferred = p.owner.config.Budget.reserveDeferred(now)
	}
	if p.deferred == nil || wave.attempts != AttemptsPerWave-1 || p.mappingReady() {
		return true
	}
	delay := max(min(MappingOpportunityDelay, WaveBudget-MinimumAttemptOpportunity)-now.Sub(wave.started), 0)
	p.setResourceActive(true)
	if p.config.Native != nil {
		p.config.Native.Idle(p.ctx, [16]byte(p.key.session), p.key.path, now.Add(delay))
	}
	return p.wait(delay)
}
func (p *Path) prepareOpportunity(wave *recoveryWave) (*attemptOpportunity, *Result) {
	if !p.awaitMappingOpportunity(*wave) {
		return nil, &Result{Stopped: true}
	}
	release := func() {}
	// Native preparation owns the single bounded process queue. Custom providers
	// retain their injectable receiver capacity without adding a hidden native queue.
	if p.config.Native == nil {
		var err error
		release, err = p.owner.config.Capacity.acquire(p.ctx)
		if err != nil {
			return nil, &Result{Stopped: true}
		}
	}
	now := p.owner.config.Clock.Now()
	if wave.attempts == 0 {
		wave.started = now
	} else if wave.exhausted(now) {
		release()
		return nil, nil
	}
	refund, delay := p.reserveAttemptBudget(wave.attempts)
	if refund == nil {
		release()
		if p.config.StopAfterWave {
			return nil, &Result{Cause: ErrWaveExhausted}
		}
		if !p.wait(delay) {
			return nil, &Result{Stopped: true}
		}
		return nil, nil
	}
	if wave.attempts == 0 && p.config.Native != nil {
		p.config.Native.BeginWave([16]byte(p.key.session), p.key.path)
	}
	binding, err := p.freshBinding()
	if err != nil {
		refund(0)
		release()
		return nil, &Result{Scope: protocolsession.PeerFailurePathTerminal, Cause: err}
	}
	return p.prepareProvider(wave, binding, refund, release), nil
}
func (p *Path) reserveAttemptBudget(attempts int) (func(time.Duration), time.Duration) {
	ready := p.consumeMapping()
	if p.deferred != nil && (ready || attempts == AttemptsPerWave-1) {
		return p.takeDeferred(), 0
	}
	refund, delay := p.owner.config.Budget.reserve(p.owner.config.Clock.Now())
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
	if p.sequence == ^uint64(0) {
		return v2signal.Binding{}, ErrPathTerminal
	}
	p.sequence++
	binding := v2signal.Binding{PeerPathID: p.key.path, AttemptSequence: p.sequence}
	p.owner.mu.Lock()
	_, err := io.ReadFull(p.owner.config.Random, binding.AttemptID[:])
	p.owner.mu.Unlock()
	if err != nil || binding.Validate() != nil {
		return binding, errors.Join(ErrConfig, err)
	}
	return binding, nil
}
func (p *Path) afterAttempt(wave *recoveryWave, result Result, admitted bool) *Result {
	p.setResourceActive(p.deferred != nil)
	if p.currentDemand() == BrowseDemand && p.ctx.Err() == nil && result.Scope != protocolsession.PeerFailureSessionTerminal && result.Scope != protocolsession.PeerFailurePathTerminal {
		if !p.awaitContentDemand() {
			return &Result{Stopped: true}
		}
		*wave = recoveryWave{started: p.owner.config.Clock.Now()}
		return nil
	}
	if result.Stopped || result.Scope == protocolsession.PeerFailureSessionTerminal || result.Scope == protocolsession.PeerFailurePathTerminal {
		return &result
	}
	if admitted {
		*wave = recoveryWave{started: p.owner.config.Clock.Now()}
		return nil
	}
	delay := time.Duration(wave.attempts) * time.Second
	if p.deferred != nil && p.config.Native != nil {
		p.config.Native.Idle(p.ctx, [16]byte(p.key.session), p.key.path, p.owner.config.Clock.Now().Add(delay))
	}
	if !p.wait(delay) {
		return &Result{Stopped: true}
	}
	return nil
}

// Finished prewarm work stays dormant until the same intent acquires content
// demand. This preserves the one speculative attempt without making a slow
// catalog browse permanently disable later direct recovery.
func (p *Path) awaitContentDemand() bool {
	for p.currentDemand() == BrowseDemand {
		select {
		case <-p.ctx.Done():
			return false
		case <-p.wake:
			p.mu.Lock()
			retired := p.retired
			p.mu.Unlock()
			if retired {
				return false
			}
		}
	}
	return p.ctx.Err() == nil && p.currentDemand() == ContentDemand
}

func (p *Path) executeStart(start Starter, binding v2signal.Binding, opportunity time.Duration, refund func(time.Duration), release func()) (Result, bool) {
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
			release()
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
		closeAttempt(attempt)
		return Result{Cause: context.DeadlineExceeded, Stopped: p.ctx.Err() != nil}, false
	}
	if err != nil || attempt == nil {
		return Result{Cause: err}, false
	}
	p.mu.Lock()
	p.current = attempt
	p.mu.Unlock()
	p.resourceDemand(false)
	defer func() {
		_ = attempt.Close()
		p.mu.Lock()
		p.current = nil
		p.lane = sessionruntime.LaneIdentity{}
		p.mu.Unlock()
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
	admitted := false
	for {
		select {
		case <-p.ctx.Done():
			return Result{Stopped: true}, admitted
		case <-p.wake:
			if p.consumeRestart() {
				return Result{Cause: errors.New("network generation replaced")}, admitted
			}
			switch p.currentDemand() {
			case NoDemand:
				return Result{Stopped: true}, admitted
			case ContentDemand:
				if retention != nil {
					retention.Stop()
					retainUntil = nil
				}
			}
		case <-deadline:
			// The enclosing attempt cap never resets when the provider advances stages.
			cancel()
			return Result{Cause: context.DeadlineExceeded}, admitted
		case <-retainUntil:
			if p.currentDemand() != ContentDemand {
				return Result{}, admitted
			}
			retainUntil = nil
		case <-ready:
			ready = nil
			admitted = true
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
			return Result{Scope: outcome.RecoveryScope(), Cause: outcome.RetainedCause(), Stopped: outcome.LocallyCanceled() && p.ctx.Err() != nil}, admitted
		}
	}
}

func closeAttempt(attempt Attempt) {
	if attempt != nil {
		_ = attempt.Close()
	}
}
