package turnpath

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/session/turncontrol"
)

const (
	TransportConnectBudget        = 12 * time.Second
	TransportEvaluationBudget     = 20 * time.Second
	TransportEvaluationBytes      = 8 << 20
	TransportEvaluationDelay      = 10 * time.Second
	TransportEvaluationCooldown   = 30 * time.Second
	TransportReevaluationInterval = 2 * time.Minute
	TransportImprovementRatio     = 1.25
	TransportImprovementHold      = 2 * time.Second
	minimumExplorationHorizon     = 2 * time.Second
	minimumEvaluationContent      = 256 << 10
)

type pathIntent string

const (
	initialPath    pathIntent = "initial"
	renewalPath    pathIntent = "renewal"
	recoveryPath   pathIntent = "recovery"
	evaluationPath pathIntent = "evaluation"
)

type transportSelection struct {
	available      []iceconfig.RelayTransport
	preferred      iceconfig.RelayTransport
	failed         map[iceconfig.RelayTransport]bool
	evaluated      map[iceconfig.RelayTransport]bool
	nextEvaluation time.Time
	remoteNetwork  [16]byte
}

func (p *transportSelection) choose(intent pathIntent, current iceconfig.RelayTransport) iceconfig.RelayTransport {
	if intent != evaluationPath {
		for _, preferred := range []iceconfig.RelayTransport{current, p.preferred} {
			if slices.Contains(p.available, preferred) && !p.failed[preferred] {
				return preferred
			}
		}
	}
	for _, candidate := range p.available {
		if !p.failed[candidate] && (intent != evaluationPath || candidate != current && !p.evaluated[candidate]) {
			return candidate
		}
	}
	return ""
}

type transportTrial struct {
	predecessorLane turncontrol.Lane
	remoteNetwork   [16]byte
	localNetwork    uint64
	baselineRate    float64
	betterSince     time.Time
	lastSequence    uint64
}

func (s *Sender) evaluationDue(service *service, now time.Time) bool {
	current := service.current
	if current == nil || len(service.selection.available) < 2 || now.Sub(current.created) < TransportEvaluationDelay ||
		now.Before(service.selection.nextEvaluation) || s.continuityDue(now) {
		return false
	}
	quality := s.measureQuality(current, now)
	if !quality.measured || quality.latest.PendingBytes == 0 {
		return false
	}
	// Only already-admitted content work justifies exploration. An idle download,
	// directory browse or nearly drained queue cannot buy a speculative socket.
	if quality.rate > 0 && float64(quality.latest.PendingBytes)/quality.rate < minimumExplorationHorizon.Seconds() {
		return false
	}
	if service.selection.choose(evaluationPath, current.transport) != "" {
		return true
	}
	service.selection.evaluated = nil
	service.selection.failed = nil
	service.selection.nextEvaluation = now.Add(TransportReevaluationInterval)
	return false
}

func (s *Sender) prepareTrial(service *service) *transportTrial {
	quality := s.measureQuality(service.current, s.config.Now())
	return &transportTrial{predecessorLane: quality.latest.Lane, remoteNetwork: quality.latest.NetworkGeneration,
		localNetwork: s.networkGeneration, baselineRate: quality.rate}
}

func (s *Sender) evaluationWins(service *service, pending *delegation, now time.Time) bool {
	trial := pending.trial
	if trial == nil {
		return true
	}
	current := s.measureQuality(service.current, now)
	candidate := s.measureQuality(pending, now)
	if !candidate.measured || candidate.latest.ContentBytes < minimumEvaluationContent ||
		current.latest.Lane != trial.predecessorLane || candidate.latest.NetworkGeneration != trial.remoteNetwork ||
		current.latest.NetworkGeneration != trial.remoteNetwork || trial.localNetwork != s.networkGeneration {
		trial.betterSince = time.Time{}
		return false
	}
	// A contender can take work away from its predecessor. Retain the pre-trial
	// baseline so starving the predecessor cannot manufacture a speed advantage.
	reference := trial.baselineRate
	if current.measured {
		reference = max(reference, current.rate)
	}
	if candidate.rate <= reference*TransportImprovementRatio {
		trial.betterSince = time.Time{}
		return false
	}
	if candidate.latest.SampleSequence == trial.lastSequence {
		return false
	}
	trial.lastSequence = candidate.latest.SampleSequence
	if trial.betterSince.IsZero() {
		trial.betterSince = now
		return false
	}
	if now.Sub(trial.betterSince) < TransportImprovementHold {
		return false
	}
	s.selectionEvent(service, pending, "measured_improvement", reference, candidate.rate)
	return true
}

func (s *Sender) maintainSelection(ctx context.Context, service *service, now time.Time) {
	// The shared overlap slot belongs to continuity first, including while a
	// speculative credential request is still waiting on its provider.
	continuity := s.continuityDue(now)
	if continuity && service.issuance != nil && service.issuance.intent == evaluationPath {
		service.issuance.cancel()
	}
	if current := service.current; current != nil {
		quality := s.measureQuality(current, now)
		if quality.latest.SampleSequence != 0 && service.selection.remoteNetwork != quality.latest.NetworkGeneration {
			service.selection = transportSelection{available: service.selection.available, remoteNetwork: quality.latest.NetworkGeneration}
		}
	}
	pending := service.pending
	if pending == nil || pending.trial == nil {
		return
	}
	trial := pending.trial
	current := s.measureQuality(service.current, now)
	candidate := s.measureQuality(pending, now)
	reason := ""
	switch {
	case continuity:
		reason = "continuity_required"
	case trial.localNetwork != s.networkGeneration || service.current == nil ||
		current.latest.Lane != trial.predecessorLane || current.latest.NetworkGeneration != trial.remoteNetwork:
		reason = "evaluation_scope_changed"
	case now.Sub(pending.created) >= TransportEvaluationBudget:
		reason = "evaluation_deadline"
	case candidate.latest.ContentBytes >= TransportEvaluationBytes:
		reason = "evaluation_byte_budget"
	}
	if reason != "" {
		s.selectionEvent(service, pending, reason, trial.baselineRate, candidate.rate)
		s.retire(ctx, pending, turncontrol.Unhelpful)
		service.pending = nil
		service.selection.nextEvaluation = now.Add(TransportEvaluationCooldown)
	}
}

func (s *Sender) continuityDue(now time.Time) bool {
	for _, service := range s.services {
		if current := service.current; current != nil && (current.renewalDue(now) || s.needsRecovery(current)) {
			return true
		}
	}
	return false
}

func (s *Sender) selectionFailed(service *service, value *delegation, now time.Time) {
	if value.trial != nil {
		service.selection.nextEvaluation = now.Add(TransportEvaluationCooldown)
		return
	}
	if len(service.selection.available) < 2 || value.readyLane.Valid() {
		return
	}
	if service.selection.failed == nil {
		service.selection.failed = make(map[iceconfig.RelayTransport]bool)
	}
	service.selection.failed[value.transport] = true
	if service.selection.choose(initialPath, "") != "" {
		service.retryAt = time.Time{}
	} else {
		service.selection.failed = nil
		service.retryAt = now.Add(ServiceCooldown)
	}
}

func (s *Sender) selectionEvent(service *service, path *delegation, reason string, previousRate, candidateRate float64) {
	var previous iceconfig.RelayTransport
	if service.current != nil {
		previous = service.current.transport
	}
	slog.Debug("TURN transport selection", "session_id", fmt.Sprintf("%x", s.config.SessionID),
		"path_id", fmt.Sprintf("%x", path.grant.PathID), "profile", path.grant.Profile, "reason", reason,
		"previous_transport", previous, "candidate_transport", path.transport, "previous_bytes_per_second", previousRate,
		"candidate_bytes_per_second", candidateRate, "network_generation", s.networkGeneration)
	s.observe(path, MilestoneTransportSelection, reason)
}
