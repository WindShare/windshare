// Package icepolicy owns trusted, transport-independent ICE selection policy.
// Construct pools only from build, deployment, or explicit local configuration.
package icepolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	EndpointsPerProfile     = 2
	EndpointsPerWave        = 4
	MaximumCatalogEndpoints = 512
	EndpointFailureCooldown = 30 * time.Second
)

var ErrConfiguration = errors.New("invalid trusted ICE endpoint configuration")

type Endpoint struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Region        string `json:"region"`
	FailureDomain string `json:"failureDomain"`
	Provider      string `json:"provider"`
	Family        string `json:"family"`
	Trust         string `json:"trust"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
}

// ICEEndpointPool keeps catalog entries separate from enabled trusted endpoints.
type ICEEndpointPool struct{ endpoints []Endpoint }

func NewICEEndpointPool(entries []Endpoint) (ICEEndpointPool, error) {
	if len(entries) > MaximumCatalogEndpoints {
		return ICEEndpointPool{}, ErrConfiguration
	}
	seen := make(map[string]bool)
	for _, endpoint := range entries {
		if endpoint.ID == "" || seen[endpoint.ID] || !validEndpointURL(endpoint.URL) ||
			(endpoint.Family != "ipv4" && endpoint.Family != "ipv6" && endpoint.Family != "any") ||
			(endpoint.Trust != "reviewed" && endpoint.Trust != "local" && endpoint.Trust != "unreviewed") ||
			(endpoint.Enabled && endpoint.Trust == "unreviewed") || endpoint.Priority < 0 {
			return ICEEndpointPool{}, ErrConfiguration
		}
		seen[endpoint.ID] = true
	}
	return ICEEndpointPool{endpoints: append([]Endpoint(nil), entries...)}, nil
}

// ParseLocalConfiguration never resolves or contacts an endpoint.
func ParseLocalConfiguration(data []byte) (ICEEndpointPool, error) {
	var entries []Endpoint
	if err := json.Unmarshal(data, &entries); err != nil {
		return ICEEndpointPool{}, fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	return NewICEEndpointPool(entries)
}

func validEndpointURL(raw string) bool {
	if !strings.HasPrefix(raw, "stun:") || strings.ContainsAny(raw, "/?#@ \t\r\n") {
		return false
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(raw, "stun:"))
	if err != nil || host == "" {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535
}

type SelectionRequest struct {
	NetworkGenerationID string
	WaveID              string
	Sequence            uint64
	Now                 time.Time
	UsedEndpointIDs     []string
	UsedFailureDomains  []string
	IPv4                bool
	IPv6                bool
}

type EndpointFact struct {
	EndpointID  string
	FailedUntil time.Time
}
type ProfileFact struct {
	ProfileID               string
	ServerReflexiveProduced bool
	FirstCandidateDelay     time.Duration
}
type GenerationFacts struct {
	NetworkGenerationID string
	Endpoints           []EndpointFact
	Profiles            []ProfileFact
}

// AttemptICEProfile is a snapshot: accessors copy slices so a live attempt cannot
// change configuration when the owner's pool or observations change.
type AttemptICEProfile struct {
	id, generation string
	endpoints      []Endpoint
}

func (p AttemptICEProfile) ID() string                  { return p.id }
func (p AttemptICEProfile) NetworkGenerationID() string { return p.generation }
func (p AttemptICEProfile) EndpointIDs() []string {
	out := make([]string, len(p.endpoints))
	for i, e := range p.endpoints {
		out[i] = e.ID
	}
	return out
}
func (p AttemptICEProfile) URLs() []string {
	out := make([]string, len(p.endpoints))
	for i, e := range p.endpoints {
		out[i] = e.URL
	}
	return out
}
func (p AttemptICEProfile) FailureDomains() []string {
	out := make([]string, len(p.endpoints))
	for i, e := range p.endpoints {
		out[i] = e.FailureDomain
	}
	return out
}

// RebindAttemptProfile rechecks an existing wave's membership against a new
// network generation without granting another endpoint exploration allowance.
func RebindAttemptProfile(profile AttemptICEProfile, request SelectionRequest, facts GenerationFacts) AttemptICEProfile {
	endpoints := make([]Endpoint, 0, len(profile.endpoints))
	for _, endpoint := range profile.endpoints {
		if endpointEligible(endpoint, request, facts, nil, nil) {
			endpoints = append(endpoints, endpoint)
		}
	}
	return profileFromEndpoints(endpoints, request)
}

func profileFromEndpoints(endpoints []Endpoint, request SelectionRequest) AttemptICEProfile {
	ids := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		ids[i] = endpoint.ID
	}
	seed := request.NetworkGenerationID + "|" + request.WaveID
	return AttemptICEProfile{id: fmt.Sprintf("ice-%08x", policyHash(seed+"|"+strconv.FormatUint(request.Sequence, 10)+"|"+strings.Join(ids, ","))), generation: request.NetworkGenerationID, endpoints: endpoints}
}

func SelectAttemptProfile(pool ICEEndpointPool, request SelectionRequest, facts GenerationFacts) AttemptICEProfile {
	used := make(map[string]bool)
	domains := make(map[string]bool)
	for _, id := range request.UsedEndpointIDs {
		used[id] = true
	}
	for _, domain := range request.UsedFailureDomains {
		if domain != "" {
			domains[domain] = true
		}
	}
	remaining := min(EndpointsPerProfile, max(0, EndpointsPerWave-len(used)))
	candidates := make([]Endpoint, 0, len(pool.endpoints))
	for _, endpoint := range pool.endpoints {
		if endpointEligible(endpoint, request, facts, used, domains) {
			candidates = append(candidates, endpoint)
		}
	}
	seed := request.NetworkGenerationID + "|" + request.WaveID
	sort.Slice(candidates, func(i, j int) bool { return endpointLess(candidates[i], candidates[j], seed) })
	selected := make([]Endpoint, 0, remaining)
	for len(candidates) > 0 && len(selected) < remaining {
		index := diverseEndpointIndex(candidates, selected)
		selected = append(selected, candidates[index])
		candidates = append(candidates[:index], candidates[index+1:]...)
	}
	return profileFromEndpoints(selected, request)
}

func endpointEligible(endpoint Endpoint, request SelectionRequest, facts GenerationFacts, used, domains map[string]bool) bool {
	if !endpoint.Enabled || used[endpoint.ID] || (endpoint.Family == "ipv4" && !request.IPv4) || (endpoint.Family == "ipv6" && !request.IPv6) || (!request.IPv4 && !request.IPv6) {
		return false
	}
	// A backup explores a new failure domain when that fact is known.
	if endpoint.FailureDomain != "" && domains[endpoint.FailureDomain] {
		return false
	}
	if facts.NetworkGenerationID != request.NetworkGenerationID {
		return true
	}
	for _, fact := range facts.Endpoints {
		if fact.EndpointID == endpoint.ID && request.Now.Before(fact.FailedUntil) {
			return false
		}
	}
	return true
}
func endpointLess(a, b Endpoint, seed string) bool {
	if a.Trust != b.Trust {
		return a.Trust == "reviewed"
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	ah, bh := policyHash(seed+"|"+a.ID), policyHash(seed+"|"+b.ID)
	if ah != bh {
		return ah < bh
	}
	return a.ID < b.ID
}
func diverseEndpointIndex(candidates, selected []Endpoint) int {
	if len(selected) == 0 {
		return 0
	}
	index, best := 0, -1
	for i, endpoint := range candidates {
		score := 0
		if endpoint.FailureDomain != "" && endpoint.FailureDomain != selected[0].FailureDomain {
			score += 2
		}
		if endpoint.Region != "" && endpoint.Region != selected[0].Region {
			score++
		}
		if score > best {
			index, best = i, score
		}
	}
	return index
}
func policyHash(value string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return h.Sum32()
}
