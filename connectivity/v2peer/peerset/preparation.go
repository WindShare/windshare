package peerset

import (
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
)

func (p *Path) prepareProvider(wave *recoveryWave, binding v2signal.Binding, limit time.Duration, refund func(time.Duration)) (*attemptOpportunity, error) {
	remaining := wave.remaining(p.owner.config.Clock.Now())
	minimum := p.minimumOpportunity()
	if remaining < minimum {
		return nil, ErrWaveExhausted
	}
	opportunity := &attemptOpportunity{binding: binding, start: p.config.Start, budget: min(limit, remaining), refund: refund}
	if p.config.Prepare == nil {
		return opportunity, nil
	}
	// Queueing spends wave time. At the boundary a ready permit can still
	// fund the full checking opportunity, while a busy gate must decline immediately.
	wait := remaining - minimum
	prepared, err := p.config.Prepare(p.ctx, binding, wait)
	remaining = wave.remaining(p.owner.config.Clock.Now())
	if err == nil && (prepared.Start == nil || prepared.Close == nil) {
		err = ErrConfig
	}
	if cause := p.ctx.Err(); cause != nil {
		err = errors.Join(cause, err)
	} else if remaining < minimum || (remaining == minimum && err != nil) {
		err = errors.Join(ErrWaveExhausted, err)
	}
	if err != nil {
		if prepared.Close != nil {
			prepared.Close()
		}
		return nil, err
	}
	opportunity.start = prepared.Start
	opportunity.close = prepared.Close
	opportunity.budget = min(limit, remaining)
	return opportunity, nil
}

func (p *Path) executePrepared(opportunity *attemptOpportunity) (Result, attemptProgress) {
	if opportunity.close != nil {
		defer opportunity.close()
	}
	return p.executeStart(opportunity.start, opportunity.binding, opportunity.budget, opportunity.refund)
}
