package turnissuer

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type source struct {
	relayconfig.TURNService
	origin, access string
	advertised     bool
}

// Sources freezes configuration before the asynchronous share starts.
type Sources struct {
	ordered []source
}

func SelectSources(manual []relayconfig.TURNService, selected []relayprofile.Registration, profiles []relayprofile.Candidate) (Sources, error) {
	if err := relayconfig.ValidateTURNServices(manual); err != nil {
		return Sources{}, err
	}
	var sources Sources
	ids := map[string]bool{}
	for _, s := range relayconfig.CloneTURNServices(manual) {
		sources.ordered = append(sources.ordered, source{TURNService: s})
		ids[s.ID] = true
	}
	byURL := make(map[string]relayprofile.Candidate)
	for _, s := range profiles {
		u, err := v2.NormalizeRelayEndpoint(s.PublicURL)
		if err == nil {
			byURL[u.DialURL] = s
		}
	}
	for _, active := range selected {
		u, err := v2.NormalizeRelayEndpoint(active.PublicURL)
		if err != nil {
			continue
		}
		s := byURL[u.DialURL]
		origin, ok := managedOrigin(s.PublicURL)
		if !ok || s.AccessKey == "" {
			continue
		}
		digest := sha256.Sum256([]byte(u.DialURL))
		id := "managed-" + hex.EncodeToString(digest[:12])
		for ids[id] {
			id += "-"
		}
		ids[id] = true
		sources.ordered = append(sources.ordered, source{ID: id, Kind: "managed", Role: relayconfig.RoleFallback, origin: origin, access: s.AccessKey, advertised: s.ManagedTurn})
	}
	return sources, nil
}

// Confirmed services precede speculative candidates. Reserve an escalation slot
// only for a confirmed opposite role, so an ordinary private relay cannot crowd
// out a working parallel service.
func activeServices(available []source, unknown map[string]bool, request turngrant.IssueRequest) []source {
	selected := slices.Clone(available)
	if len(request.ServiceIDs) > 0 {
		selected = slices.DeleteFunc(selected, func(s source) bool { return !slices.Contains(request.ServiceIDs, s.ID) })
	} else if len(available) > p.MaxGrantsPerMessage {
		selected = selected[:p.MaxGrantsPerMessage]
		role := selected[0].EffectiveRole()
		if !slices.ContainsFunc(selected, func(s source) bool { return s.EffectiveRole() != role }) {
			for _, s := range available[p.MaxGrantsPerMessage:] {
				if !unknown[s.ID] && s.EffectiveRole() != role {
					selected[len(selected)-1] = s
					break
				}
			}
		}
	}
	if request.Mode == p.TURNParallel {
		selected = slices.DeleteFunc(selected, func(s source) bool { return s.EffectiveRole() == relayconfig.RoleFallback })
	}
	return selected
}

// Known targets start issuing immediately. A full escalation can use the
// remaining places for newly confirmed endpoints, including the last parallel
// place when no fallback has been confirmed yet.
func readyTargets(request turngrant.IssueRequest, targets []source, unknown map[string]bool) []source {
	if request.Mode != p.TURNAll || len(request.ServiceIDs) != 0 || len(unknown) == 0 {
		return targets
	}
	for i, target := range targets {
		if unknown[target.ID] {
			return targets[:i]
		}
	}
	if len(targets) == p.MaxGrantsPerMessage && !slices.ContainsFunc(targets, func(s source) bool { return s.EffectiveRole() == relayconfig.RoleFallback }) {
		return targets[:len(targets)-1]
	}
	return targets
}
