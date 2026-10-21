package turnpath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
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
			if current.cancel != nil {
				current.cancel()
			}
			current.retryAt = time.Time{}
		}
		current.profile = profile
		services = append(services, current)
	}
	for _, removed := range previous {
		if removed.cancel != nil {
			removed.cancel()
		}
		for _, path := range []*delegation{removed.current, removed.pending} {
			if path != nil {
				s.retire(ctx, path, turncontrol.ProviderRevoked)
			}
		}
		removed.current, removed.pending = nil, nil
	}
	s.services, s.inventory = services, next
	slog.Debug("TURN sender configuration changed", "session_id", fmt.Sprintf("%x", s.config.SessionID),
		"config_revision", next.Revision, "profiles", len(services))
}

func (s *Sender) discardReason(result issued) string {
	switch {
	case !slices.Contains(s.services, result.service), result.profileRevision != result.service.profile.Revision, errors.Is(result.err, ErrSuperseded):
		return "configuration_superseded"
	case result.generation != s.generation:
		return "demand_superseded"
	case !s.config.Now().Before(s.demandUntil):
		return "demand_ended"
	case result.requestCancellation != nil:
		return "request_canceled"
	default:
		return ""
	}
}

func (s *Sender) observeIssuance(result issued, milestone, reason string) {
	identity := result.service.profile.Identity
	s.observe(&delegation{grant: turncontrol.Grant{PathID: result.path, Provider: identity.Provider,
		Account: identity.Account, Profile: identity.Profile}}, milestone, reason)
}
