// Package relayplan makes bounded, deterministic relay choices from supplied facts.
package relayplan

import (
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
)

const (
	MaximumEndpoints           = 8
	MaximumNetworkCandidates   = 4
	TargetNetworkRegistrations = 2
	MaximumConcurrentAttempts  = 4
	ObservationAge             = 15 * time.Minute
)

var ErrConfiguration = errors.New("invalid relay plan")

type Candidate struct {
	URL          string
	Identity     string
	Alias        string
	Zone         string
	Region       string
	Provider     string
	Operator     string
	CapacityMbps uint32
	Canary       bool
	Configured   bool
}

type Observation struct {
	Identity          string
	NetworkGeneration string
	At                time.Time
	Admitted          bool
	Duration          time.Duration
	CooldownUntil     time.Time
}

type Policy struct {
	ConfiguredOnly bool
	// Only measured pair coverage permits preferring two global nodes for a
	// global primary. Exit-location guesses cannot establish receiver reachability.
	GlobalPairCoversMainland bool
	Admit                    func(nodedirectory.Node) bool
}

type Input struct {
	Configured        []string
	Directory         nodedirectory.Payload
	Observations      []Observation
	NetworkGeneration string
	Now               time.Time
	Policy            Policy
}

type Filter struct{ Endpoint, Reason string }
type Decision struct {
	DirectoryGeneration uint64
	DirectorySerial     uint64
	NetworkGeneration   string
	ConfiguredOnly      bool
	Candidates          []string
	Filters             []Filter
}

type Plan struct {
	Configured []Candidate
	Network    []Candidate
	Policy     Policy
	Decision   Decision
}

func Build(input Input) (Plan, error) {
	plan := Plan{Policy: input.Policy, Decision: Decision{DirectoryGeneration: input.Directory.Generation,
		DirectorySerial: input.Directory.Serial, NetworkGeneration: input.NetworkGeneration, ConfiguredOnly: input.Policy.ConfiguredOnly}}
	if len(input.Configured) > MaximumEndpoints {
		return Plan{}, ErrConfiguration
	}
	seen := map[string]bool{}
	for _, raw := range input.Configured {
		endpoint, err := networkendpoint.NormalizeRelay(raw)
		if err != nil {
			return Plan{}, err
		}
		if seen[endpoint.IdentityURL] {
			return Plan{}, ErrConfiguration
		}
		seen[endpoint.IdentityURL] = true
		plan.Configured = append(plan.Configured, Candidate{URL: raw, Identity: endpoint.IdentityURL, Configured: true})
	}
	available := min(MaximumNetworkCandidates, MaximumEndpoints-len(plan.Configured))
	if input.Policy.ConfiguredOnly || available == 0 {
		return finish(plan), nil
	}
	observations := recent(input)
	var candidates []Candidate
	for _, node := range input.Directory.Nodes {
		endpoint, err := networkendpoint.NormalizeRelay(node.Relay)
		if err != nil {
			return Plan{}, err
		}
		reason := ""
		switch {
		case seen[endpoint.IdentityURL]:
			reason = "configured_or_duplicate"
		case !node.HasRole("relay"):
			reason = "relay_disabled"
		case node.Status == nodedirectory.Draining:
			reason = "draining"
		case input.Policy.Admit != nil && !input.Policy.Admit(node):
			reason = "admission_policy"
		case input.Now.Before(observations[endpoint.IdentityURL].CooldownUntil):
			reason = "cooldown"
		}
		if reason != "" {
			plan.Decision.Filters = append(plan.Decision.Filters, Filter{Endpoint: node.Relay, Reason: reason})
			continue
		}
		seen[endpoint.IdentityURL] = true
		candidates = append(candidates, Candidate{URL: node.Relay, Identity: endpoint.IdentityURL, Alias: node.Alias, Zone: node.Zone,
			Region: node.Region, Provider: node.Provider, Operator: node.Operator, CapacityMbps: node.CapacityMbps, Canary: node.Status == nodedirectory.Canary})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return less(candidates[i], candidates[j], observations) })
	if len(candidates) == 0 {
		return finish(plan), nil
	}
	plan.Network = append(plan.Network, candidates[0])
	candidates = candidates[1:]
	// Reserve diversity before racing; a fast, homogeneous prefix must not fill
	// every published fallback slot as the directory grows.
	if available > 1 && len(candidates) > 0 {
		index := BackupIndex(plan.Network[0], candidates, input.Policy)
		plan.Network = append(plan.Network, candidates[index])
		candidates = append(candidates[:index], candidates[index+1:]...)
	}
	if len(plan.Network) < available {
		if index := slices.IndexFunc(candidates, func(candidate Candidate) bool { _, measured := observations[candidate.Identity]; return !measured }); index >= 0 {
			plan.Network = append(plan.Network, candidates[index])
			candidates = append(candidates[:index], candidates[index+1:]...)
		}
	}
	for _, candidate := range candidates {
		if len(plan.Network) == available {
			break
		}
		plan.Network = append(plan.Network, candidate)
	}
	return finish(plan), nil
}

func finish(plan Plan) Plan {
	for _, candidate := range plan.Configured {
		plan.Decision.Candidates = append(plan.Decision.Candidates, candidate.URL)
	}
	for _, candidate := range plan.Network {
		plan.Decision.Candidates = append(plan.Decision.Candidates, candidate.URL)
	}
	return plan
}

func recent(input Input) map[string]Observation {
	result := map[string]Observation{}
	for _, observation := range input.Observations {
		if observation.NetworkGeneration != input.NetworkGeneration || observation.At.After(input.Now) || input.Now.Sub(observation.At) > ObservationAge {
			continue
		}
		if previous, ok := result[observation.Identity]; !ok || observation.At.After(previous.At) {
			result[observation.Identity] = observation
		}
	}
	return result
}

func less(a, b Candidate, facts map[string]Observation) bool {
	left, lok := facts[a.Identity]
	right, rok := facts[b.Identity]
	rank := func(known bool, fact Observation) int {
		if !known {
			return 1
		}
		if fact.Admitted {
			return 2
		}
		return 0
	}
	if l, r := rank(lok, left), rank(rok, right); l != r {
		return l > r
	}
	if a.Canary != b.Canary {
		return !a.Canary
	}
	if a.CapacityMbps != b.CapacityMbps {
		return a.CapacityMbps > b.CapacityMbps
	}
	if lok && rok && left.Admitted && right.Admitted && left.Duration != right.Duration {
		return left.Duration < right.Duration
	}
	return a.Identity < b.Identity
}

func BackupIndex(primary Candidate, candidates []Candidate, policy Policy) int {
	best, score := 0, -1
	preferredZone := "global"
	if primary.Zone == "global" && !policy.GlobalPairCoversMainland {
		preferredZone = "cn"
	}
	for index, candidate := range candidates {
		value := 0
		if candidate.Zone == preferredZone {
			value += 4
		}
		if candidate.Provider != "" && candidate.Provider != primary.Provider {
			value += 2
		}
		if candidate.Operator != "" && candidate.Operator != primary.Operator {
			value++
		}
		if value > score {
			best, score = index, value
		}
	}
	return best
}
