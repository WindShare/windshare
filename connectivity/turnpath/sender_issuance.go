package turnpath

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type credentialRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
	intent pathIntent
}

func (s *Sender) acquire(ctx context.Context, service *service) {
	var path [16]byte
	if _, err := io.ReadFull(s.config.Random, path[:]); err != nil || path == ([16]byte{}) {
		service.retryAt = s.config.Now().Add(ServiceCooldown)
		return
	}
	sequence := s.sequence
	generation := s.generation
	profile := service.profile
	current := service.current
	recovery := s.needsRecovery(current) && !current.renewalDue(s.config.Now())
	intent := initialPath
	var transport iceconfig.RelayTransport
	var predecessor [16]byte
	var trial *transportTrial
	if current != nil {
		predecessor, transport = current.grant.PathID, current.transport
		switch {
		case current.renewalDue(s.config.Now()):
			intent = renewalPath
		case recovery:
			intent = recoveryPath
		default:
			intent, trial = evaluationPath, s.prepareTrial(service)
		}
	}
	transport = service.selection.choose(intent, transport)
	if intent == evaluationPath {
		if service.selection.evaluated == nil {
			service.selection.evaluated = make(map[iceconfig.RelayTransport]bool)
		}
		service.selection.evaluated[transport] = true
		service.selection.nextEvaluation = s.config.Now().Add(TransportEvaluationCooldown)
	}
	if recovery {
		service.recoveryAttempts++
	}
	issuanceBudget := ReplacementBudget
	if intent == evaluationPath {
		issuanceBudget = TransportConnectBudget
	}
	child, cancel := context.WithTimeout(ctx, issuanceBudget)
	request := &credentialRequest{ctx: child, cancel: cancel, intent: intent}
	service.issuance = request
	s.observe(&delegation{grant: turncontrol.Grant{PathID: path, Provider: service.profile.Identity.Provider,
		Account: service.profile.Identity.Account, Profile: service.profile.Identity.Profile}}, MilestoneIssuanceStarted, "")
	s.work.Go(func() {
		subject := Subject{SessionID: s.config.SessionID, PathID: path}
		var lease *Lease
		var err error
		if intent == renewalPath {
			lease, err = current.lease.Renew(child, subject)
		} else {
			lease, err = s.config.Accounts.Acquire(child, profile.Identity.Profile, subject)
		}
		result := issued{service: service, path: path, sequence: sequence, generation: generation, profileRevision: profile.Revision,
			request: request, lease: lease, err: err, intent: intent, transport: transport, predecessor: predecessor, trial: trial}
		select {
		case s.completed <- result:
		case <-ctx.Done():
			if lease != nil {
				lease.Close()
			}
		}
	})
}

func (s *Sender) install(ctx context.Context, result issued) {
	s.refreshConfiguration(ctx)
	s.refreshRecoveryNetwork()
	service := result.service
	if result.err == nil {
		result.err = context.Cause(result.request.ctx)
	}
	reason := s.discardReason(result)
	result.request.cancel()
	service.issuance = nil
	if reason != "" {
		if result.lease != nil {
			result.lease.Close()
		}
		s.observeIssuance(result, MilestoneIssuanceDiscarded, reason)
		// Completion releases the single in-flight slot. Reconcile fresh demand
		// now rather than imposing either a failure cooldown or a maintenance tick.
		s.maintain(ctx)
		return
	}
	if result.err != nil {
		if result.lease != nil {
			result.lease.Close()
		}
		if result.intent == evaluationPath {
			service.selection.nextEvaluation = s.config.Now().Add(TransportEvaluationCooldown)
			s.observeIssuance(result, MilestoneEvaluationUnavailable, result.err.Error())
			return
		}
		service.retryAt = s.config.Now().Add(ServiceCooldown)
		s.observeIssuance(result, MilestoneIssuanceFailed, result.err.Error())
		s.send(ctx, turncontrol.Unavailable{Authority: s.header(), Profile: service.profile.Identity.Profile,
			RetryAfter: ServiceCooldown, Reason: result.err.Error()})
		return
	}
	relay := result.lease.Relay()
	service.selection.available = relay.Transports()
	transport := result.transport
	if transport == "" {
		transport = service.selection.choose(result.intent, "")
	}
	narrowed, err := relay.WithTransport(transport)
	if err != nil {
		result.lease.Close()
		service.deferIssuance(result.intent, s.config.Now())
		s.observeIssuance(result, MilestoneIssuanceDiscarded, "transport_unavailable")
		return
	}
	credential := relay.Credentials()
	grant := turncontrol.Grant{Authority: turncontrol.Header{SessionID: s.config.SessionID, OwnerPathID: s.owner, Sequence: result.sequence},
		PathID: result.path, Provider: relay.Identity().Provider, Account: relay.Identity().Account, Profile: relay.Identity().Profile,
		Username: credential.Username(), Password: credential.Password(), URLs: narrowed.URLs(), CredentialKind: turncontrol.Fixed}
	if !credential.ExpiresAt().IsZero() {
		grant.CredentialKind = turncontrol.Expiring
		grant.ValidFor = min(credential.ExpiresAt().Sub(s.config.Now()).Truncate(time.Millisecond), turncontrol.MaximumCredentialLifetime)
	}
	if service.current != nil {
		grant.Replaces = service.current.grant.PathID
	}
	value := &delegation{lease: result.lease, grant: grant, created: s.config.Now(), relay: narrowed,
		transport: transport, intent: result.intent, trial: result.trial}
	s.mu.Lock()
	s.paths[grant.PathID] = value
	s.mu.Unlock()
	service.pending = value
	if !s.send(ctx, grant) {
		s.retire(ctx, value, turncontrol.Unhelpful)
		service.pending = nil
		service.deferIssuance(result.intent, s.config.Now())
		return
	}
	s.observe(value, MilestoneCredentialsDelegated, "")
	s.selectionEvent(service, value, string(result.intent), 0, 0)
}

func (service *service) deferIssuance(intent pathIntent, now time.Time) {
	if intent == evaluationPath {
		service.selection.nextEvaluation = now.Add(TransportEvaluationCooldown)
	} else {
		service.retryAt = now.Add(ServiceCooldown)
	}
}

func (s *Sender) promote(ctx context.Context, service *service) {
	pending := service.pending
	if pending == nil || !pending.readyLane.Valid() {
		return
	}
	now := s.config.Now()
	if !now.Before(s.demandUntil) || !pending.lease.Relay().Credentials().ValidAt(now) ||
		now.Sub(pending.created) >= ReplacementBudget {
		return
	}
	select {
	case <-pending.lease.Lost():
		return
	default:
	}
	if !s.evaluationWins(service, pending, now) {
		return
	}
	// Admission revocation and promotion share one commit boundary. Checking the
	// channel's terminal signal also covers delayed attempt cleanup callbacks.
	s.mu.Lock()
	admission := pending.admission
	if admission == nil || admission.lane != pending.readyLane || !admission.live() || pending.recovery != 0 {
		s.mu.Unlock()
		return
	}
	if pending.trial != nil && (service.current == nil || service.current.admission == nil ||
		service.current.admission.lane != pending.trial.predecessorLane || !service.current.admission.live()) {
		s.mu.Unlock()
		return
	}
	old := service.current
	service.current, service.pending = pending, nil
	s.mu.Unlock()
	service.selection.preferred = pending.transport
	pending.trial = nil
	if old != nil {
		s.retire(ctx, old, turncontrol.Replaced)
	}
	s.observe(pending, MilestoneReplacementAdmitted, "")
}
