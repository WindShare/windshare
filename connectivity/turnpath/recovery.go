package turnpath

import (
	"context"
	"time"

	"github.com/windshare/windshare/core/session/turncontrol"
)

const MaximumRecoveryReplacements = 2

// Recover is safe in transport callbacks and never waits on signaling or
// issuance. Maintenance consumes the durable request even if the wake is full.
func (s *Sender) Recover(path [16]byte, reason turncontrol.RecoveryReason) {
	if reason < turncontrol.AllocationStateMismatch || reason > turncontrol.ChannelUnavailable {
		return
	}
	s.mu.Lock()
	if value := s.paths[path]; value != nil {
		if value.recovery == 0 {
			value.recovery = reason
		}
		if reason == turncontrol.AllocationStateMismatch && value.recoveryUntil.IsZero() {
			value.recovery = reason
			value.recoveryUntil = s.config.Now().Add(ReplacementBudget)
		}
	}
	s.mu.Unlock()
	select {
	case s.reconcileWake <- struct{}{}:
	default:
	}
}

func (s *Sender) applyRecovery(value turncontrol.Recover) {
	s.mu.Lock()
	path := s.paths[value.PathID]
	valid := path != nil && path.grant.Authority == value.Authority
	s.mu.Unlock()
	if valid {
		s.Recover(value.PathID, value.Reason)
	}
}

func (s *Sender) applyDemand(ctx context.Context, value turncontrol.Demand) {
	header := value.Authority
	if header.Sequence <= s.sequence {
		return
	}
	s.owner, s.sequence = header.OwnerPathID, header.Sequence
	if value.HoldFor == 0 || !s.config.Now().Before(s.demandUntil) {
		s.generation++
		for _, service := range s.services {
			service.recoveryAttempts = 0
		}
	}
	s.demandUntil = s.config.Now().Add(value.HoldFor)
	s.maintain(ctx)
}

func (s *Sender) refreshRecoveryNetwork() {
	if s.config.NetworkGeneration == nil {
		return
	}
	generation := s.config.NetworkGeneration()
	if generation == s.networkGeneration {
		return
	}
	s.networkGeneration = generation
	for _, service := range s.services {
		service.selection = transportSelection{available: service.selection.available}
		s.mu.Lock()
		for _, path := range []*delegation{service.current, service.pending} {
			if path != nil {
				path.quality.points = nil
			}
		}
		s.mu.Unlock()
		if service.recoveryAttempts != 0 {
			service.retryAt = time.Time{}
			service.recoveryAttempts = 0
		}
	}
}

func (s *Sender) recoverBudgetAfterProgress(path *delegation) {
	s.mu.Lock()
	healthy := path.recovery == 0
	s.mu.Unlock()
	// Admission alone can succeed over indications on a persistently broken
	// server. Only sustained, useful traffic on a clean path resets the budget.
	if !healthy || s.config.Now().Sub(path.created) < ServiceCooldown {
		return
	}
	for _, service := range s.services {
		if service.current == path {
			service.recoveryAttempts = 0
		}
	}
}

func (s *Sender) canAcquire(service *service, now time.Time, active, overlap int) bool {
	if service.pending != nil || service.issuance != nil || now.Before(service.retryAt) {
		return false
	}
	if service.current == nil {
		return active < MaximumActiveServices && service.recoveryAttempts < MaximumRecoveryReplacements
	}
	if overlap >= MaximumReplacementPaths {
		return false
	}
	return service.current.renewalDue(now) ||
		s.needsRecovery(service.current) && service.recoveryAttempts < MaximumRecoveryReplacements || s.evaluationDue(service, now)
}

func (s *Sender) countInitialRecoveryFailure(service *service, value *delegation) {
	if value != service.pending || value.grant.Replaces != ([16]byte{}) {
		return
	}
	s.mu.Lock()
	faulted := value.recovery != 0
	s.mu.Unlock()
	if faulted {
		// Replacements spend their budget at issuance. A first allocation can
		// fail before admission too; it must not evade the same circuit.
		service.recoveryAttempts++
	}
}

func (s *Sender) needsRecovery(value *delegation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return value != nil && value.recovery != 0
}

func (r *Receiver) Recover(path [16]byte, reason turncontrol.RecoveryReason) {
	if reason < turncontrol.AllocationStateMismatch || reason > turncontrol.ChannelUnavailable {
		return
	}
	// There are at most three delegated paths. Keeping one request per path
	// prevents callback bursts from either blocking transport or losing a fault.
	r.mu.Lock()
	if len(r.recoveries) < MaximumActiveServices+MaximumReplacementPaths {
		r.recoveries[path] = reason
	}
	r.mu.Unlock()
	select {
	case r.demands <- true:
	default:
	}
}

func (r *Receiver) requestRecoveries(ctx context.Context) {
	r.mu.Lock()
	requests := r.recoveries
	r.recoveries = make(map[[16]byte]turncontrol.RecoveryReason)
	r.mu.Unlock()
	for id, reason := range requests {
		path := r.paths[id]
		if path == nil || !r.contentDemand() {
			continue
		}
		if r.send(ctx, turncontrol.Recover{Authority: path.grant.Authority, PathID: id, Reason: reason}) {
			r.observe(path.grant, "recovery_requested", "")
		} else {
			r.mu.Lock()
			r.recoveries[id] = reason
			r.mu.Unlock()
		}
	}
}
