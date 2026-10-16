package relayv2

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/connectionliveness"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// HeartbeatConfig sets this client's probe cadence and how long its relay
// connection may stay without accepted inbound frames. Zero fields select the
// protocol values, which the relay's own silence timeout is sized against.
type HeartbeatConfig struct {
	Interval time.Duration
	Timeout  time.Duration
}

var (
	ErrHeartbeat       = connectionliveness.ErrSilent
	ErrHeartbeatConfig = errors.New("relay v2 heartbeat: invalid configuration")
)

func (c HeartbeatConfig) Normalize() (HeartbeatConfig, error) {
	if c.Interval == 0 {
		c.Interval = v2.ConnectionProbeInterval
	}
	if c.Timeout == 0 {
		c.Timeout = v2.ConnectionSilenceTimeout
	}
	// A timeout within one cadence could expire before the first probe gave an
	// otherwise idle relay anything to answer.
	if c.Interval < 0 || c.Timeout <= c.Interval {
		return HeartbeatConfig{}, ErrHeartbeatConfig
	}
	return c, nil
}

// maxTrackedProbes bounds acknowledgement correlation. On a slow downlink the
// acknowledgements can lag several probes while content keeps the connection
// alive; untracked probes lose only their round-trip trace, not acceptance.
const maxTrackedProbes = 8

type probeRecord struct {
	round, operationID uint64
	scheduledAt        time.Time
}

// probeLedger owns one connection's probe nonces. The heartbeat loop schedules,
// the writer sends, and the reader acknowledges, so they share one lock.
type probeLedger struct {
	mu           sync.Mutex
	round        uint64
	acknowledged uint64
	unsent       uint64
	tracked      []probeRecord
}

// schedule allocates the next nonce unless the previous probe still waits for
// the writer: a second queued probe would not leave the client any sooner.
func (p *probeLedger) schedule(operationID uint64, now time.Time) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unsent != 0 {
		return 0, false
	}
	p.round++
	p.unsent = p.round
	if len(p.tracked) == maxTrackedProbes {
		p.tracked = slices.Delete(p.tracked, 0, 1)
	}
	p.tracked = append(p.tracked, probeRecord{round: p.round, operationID: operationID, scheduledAt: now})
	return p.round, true
}

func (p *probeLedger) takeUnsent() (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	nonce := p.unsent
	p.unsent = 0
	return nonce, nonce != 0
}

func (p *probeLedger) newestRound() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.round
}

// acknowledge accepts the relay's newest answered nonce, which covers every
// earlier probe. A nonce already covered or never handed to the writer is a
// protocol violation rather than stale evidence.
func (p *probeLedger) acknowledge(nonce uint64) (probeRecord, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sent := p.round
	if p.unsent != 0 {
		sent--
	}
	if nonce <= p.acknowledged || nonce > sent {
		return probeRecord{}, false, ErrProtocol
	}
	p.acknowledged = nonce
	var record probeRecord
	correlated := false
	p.tracked = slices.DeleteFunc(p.tracked, func(candidate probeRecord) bool {
		if candidate.round == nonce {
			record, correlated = candidate, true
		}
		return candidate.round <= nonce
	})
	return record, correlated, nil
}

func isHeartbeatLifecycleStage(stage LifecycleStage) bool {
	return stage == LifecycleHeartbeatProbe || stage == LifecycleHeartbeatAcknowledged || stage == LifecycleHeartbeatFailed
}

func validateHeartbeatLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if hasLifecycleSession(event) || event.OperationID == 0 || event.HeartbeatRound == 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Terminal || event.Disposition != 0 || event.Dropped != 0 ||
		event.RetirementSource != LifecycleRetirementNone || event.DrainCause != LifecycleCauseNone ||
		event.Timeout <= 0 || event.Wait < 0 {
		return LifecycleContractInvalidStageFields
	}
	if (event.Stage == LifecycleHeartbeatFailed) != (event.Cause != LifecycleCauseNone) ||
		(event.Stage == LifecycleHeartbeatProbe && event.Wait != 0) {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

// heartbeatLoop probes on a fixed cadence and fails the link only after a full
// timeout without accepted inbound frames. Probe and silence decisions share
// one loop so a failure always follows at least one probe.
func (l *link) heartbeatLoop() {
	nextProbe := time.Now().Add(l.heartbeat.Interval)
	for {
		now := time.Now()
		silence := l.liveness.Silence(now)
		if silence >= l.heartbeat.Timeout {
			l.trace(heartbeatTrace(
				l.nextOperationID(), LifecycleHeartbeatFailed, l.probes.newestRound(),
				silence, l.heartbeat.Timeout, LifecycleCauseDeadline,
			))
			l.stop(ErrHeartbeat)
			return
		}
		if !now.Before(nextProbe) {
			l.probe(now)
			nextProbe = now.Add(l.heartbeat.Interval)
		}
		timer := time.NewTimer(min(nextProbe.Sub(now), l.heartbeat.Timeout-silence))
		select {
		case <-l.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (l *link) probe(now time.Time) {
	operationID := l.nextOperationID()
	round, scheduled := l.probes.schedule(operationID, now)
	if !scheduled {
		return
	}
	l.trace(heartbeatTrace(operationID, LifecycleHeartbeatProbe, round, 0, l.heartbeat.Timeout, LifecycleCauseNone))
	select {
	case l.writeWake <- struct{}{}:
	default:
	}
}

func (l *link) acknowledgeProbe(encoded []byte) error {
	ack, err := v2.ParseConnectionProbeAck(encoded)
	if err != nil {
		return ErrProtocol
	}
	record, correlated, err := l.probes.acknowledge(ack.Nonce)
	if err != nil {
		return err
	}
	if correlated {
		l.trace(heartbeatTrace(
			record.operationID, LifecycleHeartbeatAcknowledged, record.round,
			time.Since(record.scheduledAt), l.heartbeat.Timeout, LifecycleCauseNone,
		))
	}
	return nil
}

func (l *link) takeProbe() ([]byte, bool) {
	nonce, ok := l.probes.takeUnsent()
	if !ok {
		return nil, false
	}
	encoded, _ := v2.ConnectionProbe{Nonce: nonce}.MarshalBinary()
	return encoded, true
}

func heartbeatTrace(
	operationID uint64,
	stage LifecycleStage,
	round uint64,
	wait, timeout time.Duration,
	cause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		OperationID: operationID, Stage: stage, Cause: cause,
		RetirementSource: LifecycleRetirementNone, DrainCause: LifecycleCauseNone,
		HeartbeatRound: round, Wait: wait, Timeout: timeout,
	}
}
