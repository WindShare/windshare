package turnpath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/connectionpath"
	"github.com/windshare/windshare/core/session/turncontrol"
)

func (s *Sender) refreshConfiguration(ctx context.Context) {
	if s.config.Accounts == nil {
		return
	}
	next := s.config.Accounts.Inventory()
	if next.Revision == s.inventory.Revision {
		return
	}
	previous := make(map[iceconfig.Identity]*service, len(s.services))
	for _, service := range s.services {
		previous[service.profile.Identity] = service
	}
	services := make([]*service, 0, len(next.Profiles))
	for _, profile := range next.Profiles {
		current := previous[profile.Identity]
		if current == nil {
			current = &service{profile: profile}
		}
		delete(previous, profile.Identity)
		if current.profile.Revision != profile.Revision {
			if current.preparation.issuance() != nil {
				current.preparation.issuance().cancel()
			}
			current.retryAt = time.Time{}
			current.selection = transportSelection{}
		}
		current.profile = profile
		services = append(services, current)
	}
	for _, removed := range previous {
		if removed.preparation.issuance() != nil {
			removed.preparation.issuance().cancel()
		}
		for _, path := range []*delegation{removed.current, removed.preparation.pending()} {
			if path != nil {
				s.retire(ctx, path, turncontrol.ProviderRevoked)
			}
		}
		removed.current = nil
		removed.preparation.detach(removed.preparation.pending())
	}
	s.services, s.inventory = services, next
	slog.Debug("TURN sender configuration changed", "session_id", fmt.Sprintf("%x", s.config.SessionID),
		"config_revision", next.Revision, "profiles", len(services))
}

func (s *Sender) discardReason(result issued) string {
	switch {
	case !slices.Contains(s.services, result.service), result.profileRevision != result.service.profile.Revision, errors.Is(result.err, ErrSuperseded):
		return "configuration_superseded"
	case result.service.preparation.issuance() != result.request:
		return "issuance_superseded"
	case result.generation != s.generation:
		return "demand_superseded"
	case !s.config.Now().Before(s.demandUntil):
		return "demand_ended"
	case errors.Is(context.Cause(result.request.ctx), context.Canceled):
		return "request_canceled"
	case result.predecessor != ([16]byte{}) && (result.service.current == nil || result.service.current.grant.PathID != result.predecessor):
		return "predecessor_retired"
	case result.trial != nil && (result.trial.localNetwork != s.networkGeneration || result.trial.remoteNetwork != result.service.selection.remoteNetwork):
		return "evaluation_scope_changed"
	default:
		return ""
	}
}

func (s *Sender) observeIssuance(result issued, milestone connectionpath.Milestone, reason string) {
	identity := result.service.profile.Identity
	slog.Debug("TURN credential completion", "session_id", fmt.Sprintf("%x", s.config.SessionID),
		"path_id", fmt.Sprintf("%x", result.path), "service", identity,
		"preparation_phase", result.service.preparation.phase(), "reason", reason)
	s.observe(&delegation{grant: turncontrol.Grant{PathID: result.path, Provider: identity.Provider,
		Account: identity.Account, Profile: identity.Profile}}, milestone, reason)
}
