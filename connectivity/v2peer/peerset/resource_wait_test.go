package peerset

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestResourceDeferralsPreserveNegotiationAllowanceButNotWaveDeadline(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	path := &Path{owner: owner, ctx: t.Context(), key: pathKey{path: v2signal.PeerPathID{1}}, config: PathConfig{StopAfterWave: true}, demand: ContentDemand, wake: make(chan struct{}, 1), resourceChanges: make(chan struct{}, 1)}
	wave := recoveryWave{started: clock.Now()}
	started := wave.started
	for range AttemptsPerWave + 1 {
		opportunity, stop := path.prepareOpportunity(&wave)
		if stop != nil || opportunity == nil {
			t.Fatal("busy peer exhausted negotiation allowance", stop)
		}
		wave.attempts++
		wave.opportunities++
		opportunity.refund(0)
		opportunity.release()
		settled := make(chan *Result, 1)
		go func() {
			settled <- path.afterAttempt(&wave, Result{Scope: protocolsession.PeerFailureResourceDeferred}, false)
		}()
		timer := nextTimer(t, clock, ResourceRetryDelay)
		clock.advance(ResourceRetryDelay)
		timer.fire(clock.Now())
		if stop := receive(t, settled); stop != nil {
			t.Fatal(stop)
		}
		if wave.attempts != 0 || wave.started != started {
			t.Fatal("deferral reset wave or spent ICE", wave)
		}
	}
	owner.config.Budget.mu.Lock()
	remaining := owner.config.Budget.attempts
	owner.config.Budget.mu.Unlock()
	if remaining != AttemptsPerWindow {
		t.Fatal("deferral charged receive budget", remaining)
	}
	clock.advance(WaveBudget)
	if !wave.exhausted(clock.Now()) {
		t.Fatal("busy peer removed deadline")
	}
	if stop := path.nextWave(&wave); stop == nil || stop.Cause != ErrWaveExhausted {
		t.Fatal(stop)
	}
}

func TestResourceRetryRemainsCancelable(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	ctx, cancel := context.WithCancel(t.Context())
	path := &Path{owner: owner, ctx: ctx, demand: ContentDemand, wake: make(chan struct{}, 1), resourceChanges: make(chan struct{}, 1)}
	wave := recoveryWave{started: clock.Now(), attempts: 1, opportunities: 1}
	done := make(chan *Result, 1)
	go func() {
		done <- path.afterAttempt(&wave, Result{Scope: protocolsession.PeerFailureResourceDeferred}, false)
	}()
	nextTimer(t, clock, time.Second)
	cancel()
	if stop := receive(t, done); stop == nil || !stop.Stopped {
		t.Fatal(stop)
	}
}
