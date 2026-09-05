package peerset

import (
	"context"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"time"
)

func (p *Path) prepareProvider(wave *recoveryWave, binding v2signal.Binding, refund func(time.Duration), release func()) *attemptOpportunity {
	opportunity := &attemptOpportunity{binding: binding, start: p.config.Start, refund: refund, release: release}
	remaining := func() time.Duration { return WaveBudget - p.owner.config.Clock.Now().Sub(wave.started) }
	if p.config.Prepare != nil {
		// Queue time belongs to the absolute wave, not to the active attempt. Keep
		// enough time for a complete preparation/checking opportunity after admission.
		wait := min(nativepeer.ProcessQueueBudget, remaining()-MinimumAttemptOpportunity)
		prepared, err := p.waitPreparation(binding, max(wait, 0))
		if err == nil && remaining() < MinimumAttemptOpportunity {
			prepared.Close()
			err = ErrWaveExhausted
		}
		if err != nil {
			opportunity.start = func(context.Context, v2signal.Binding) (Attempt, error) { return nil, err }
		} else {
			opportunity.start = prepared.Start
			opportunity.close = prepared.Close
		}
	}
	opportunity.budget = min(AttemptBudget, max(remaining(), 0))
	return opportunity
}
func (p *Path) waitPreparation(binding v2signal.Binding, wait time.Duration) (PreparedStarter, error) {
	child, cancel := context.WithCancel(p.ctx)
	timer := p.owner.config.Clock.NewTimer(wait)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-timer.C():
			cancel()
		case <-child.Done():
		}
	}()
	prepared, err := p.config.Prepare(child, binding)
	if err == nil && (prepared.Start == nil || prepared.Close == nil) {
		if prepared.Close != nil {
			prepared.Close()
		}
		prepared = PreparedStarter{}
		err = ErrConfig
	}
	timer.Stop()
	expired := child.Err()
	cancel()
	<-done
	if expired != nil {
		if prepared.Close != nil {
			prepared.Close()
		}
		return PreparedStarter{}, expired
	}
	return prepared, err
}
func (p *Path) executePrepared(opportunity *attemptOpportunity) (Result, bool) {
	if opportunity.close != nil {
		defer opportunity.close()
	}
	return p.executeStart(opportunity.start, opportunity.binding, opportunity.budget, opportunity.refund, opportunity.release)
}
