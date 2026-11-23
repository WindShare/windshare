package relayconfig

import (
	"slices"
	"strings"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	MaxTURNServices = 8
	DefaultTURNTTL  = 1800
	MinimumTURNTTL  = 60
	MaximumTURNTTL  = 86400
	TURNStatic      = "shared_static"
	TURNCoturn      = "coturn"
	TURNCloudflare  = "cloudflare"
	RoleParallel    = "parallel"
	RoleFallback    = "fallback"
)

type TURNService struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Role         string   `json:"role,omitempty"`
	URLs         []string `json:"urls,omitempty"`
	Username     string   `json:"username,omitempty"`
	Password     string   `json:"password,omitempty"`
	SharedSecret string   `json:"shared_secret,omitempty"`
	KeyID        string   `json:"key_id,omitempty"`
	APIToken     string   `json:"api_token,omitempty"`
	TTLSeconds   int      `json:"ttl_seconds,omitempty"`
}

func (service TURNService) EffectiveRole() string {
	if service.Role != "" {
		return service.Role
	}
	if service.Kind == TURNCloudflare {
		return RoleFallback
	}
	return RoleParallel
}

func (service TURNService) TTL() int {
	if service.TTLSeconds == 0 {
		return DefaultTURNTTL
	}
	return service.TTLSeconds
}

func CloneTURNServices(services []TURNService) []TURNService {
	services = slices.Clone(services)
	for i := range services {
		services[i].URLs = slices.Clone(services[i].URLs)
	}
	return services
}

func ValidateTURNServices(services []TURNService) error {
	if len(services) > MaxTURNServices {
		return ErrConfig
	}
	ids := make(map[string]bool)
	for _, s := range services {
		if !protocolsession.ValidTURNServiceID(s.ID) || ids[s.ID] || len(s.URLs) > protocolsession.MaxURLsPerGrant {
			return ErrConfig
		}
		ids[s.ID] = true
		if role := s.EffectiveRole(); role != RoleParallel && role != RoleFallback {
			return ErrConfig
		}
		for _, url := range s.URLs {
			if !protocolsession.ValidTURNURL(url) {
				return ErrConfig
			}
		}
		if s.TTL() < MinimumTURNTTL || s.TTL() > MaximumTURNTTL {
			return ErrConfig
		}
		switch s.Kind {
		case TURNStatic:
			if len(s.URLs) == 0 || !protocolsession.ValidTURNCredential(s.Username) || !protocolsession.ValidTURNCredential(s.Password) || s.SharedSecret != "" || s.KeyID != "" || s.APIToken != "" || s.TTLSeconds != 0 {
				return ErrConfig
			}
		case TURNCoturn:
			if len(s.URLs) == 0 || s.SharedSecret == "" || s.Username != "" || s.Password != "" || s.KeyID != "" || s.APIToken != "" {
				return ErrConfig
			}
		case TURNCloudflare:
			if s.KeyID == "" || strings.ContainsAny(s.KeyID, "/%?#\\") || s.APIToken == "" || s.SharedSecret != "" || s.Username != "" || s.Password != "" {
				return ErrConfig
			}
		default:
			return ErrConfig
		}
	}
	return nil
}
