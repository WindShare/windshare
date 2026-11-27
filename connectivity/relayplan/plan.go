// Package relayplan makes bounded, deterministic relay choices from supplied facts.
package relayplan

import (
	"errors"
	"slices"
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

type Source string

const (
	Official   Source = "official"
	Configured Source = "configured"
	Community  Source = "community"
)

type Candidate struct {
	URL              string
	Identity         string
	Alias            string
	Zone             string
	Region           string
	Provider         string
	Operator         string
	history          registrationHistory
	ObservedDuration time.Duration
	CapacityMbps     uint32
	Canary           bool
	Source           Source
	Verified         bool
	IPLiteral        bool
	CooldownUntil    time.Time
	ServerRetryAt    time.Time
}

// ObservedHandshake returns the recent admitted handshake duration used for
// ranking, reporting false when the candidate was unmeasured or last failed.
func (c Candidate) ObservedHandshake() (time.Duration, bool) {
	if c.history != recentlyAdmitted {
		return 0, false
	}
	return max(c.ObservedDuration, 0), true
}

func (c Candidate) HistoryState() string {
	switch c.history {
	case recentlyAdmitted:
		return "admitted"
	case recentlyFailed:
		return "failed"
	default:
		return "unmeasured"
	}
}

// WithRegistrationFailure marks a candidate copy as failed so live backup
// arbitration within the same share applies the same ranking rule as Build.
func (c Candidate) WithRegistrationFailure() Candidate {
	c.history = recentlyFailed
	c.ObservedDuration = 0
	return c
}

// WithoutRegistrationFailure clears a failed state once a candidate completes
// a registration handshake, without inventing a measured duration sample.
func (c Candidate) WithoutRegistrationFailure() Candidate {
	if c.history == recentlyFailed {
		c.history = unmeasured
		c.ObservedDuration = 0
	}
	return c
}

type registrationHistory int8

const (
	recentlyFailed   registrationHistory = -1
	unmeasured       registrationHistory = 0
	recentlyAdmitted registrationHistory = 1
)

type CooldownReason string

const (
	CooldownNone             CooldownReason = ""
	CooldownServerBackoff    CooldownReason = "server_backoff"
	CooldownNodeRejected     CooldownReason = "node_rejected"
	CooldownTransportFailure CooldownReason = "transport_failure"
)

type Observation struct {
	Identity          string
	NetworkGeneration string
	At                time.Time
	Admitted          bool
	Duration          time.Duration
	CooldownUntil     time.Time
	CooldownReason    CooldownReason
	ServerRetryAt     time.Time
}

// ActiveAt projects historical deadlines into the current scheduling decision.
func (o Observation) ActiveAt(now time.Time) Observation {
	if !o.CooldownUntil.After(now) {
		o.CooldownUntil = time.Time{}
		o.CooldownReason = CooldownNone
	}
	if !o.ServerRetryAt.After(now) {
		o.ServerRetryAt = time.Time{}
	}
	return o
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
	Community         []Candidate
	Directory         nodedirectory.Payload
	SelectionSeed     string
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
	CooldownFallback    bool
}

type Plan struct {
	Configured []Candidate
	Network    []Candidate
	Standby    []Candidate
	Policy     Policy
	Decision   Decision
}

// DirectoryCandidates returns every directory-sourced candidate in startup wave
// order (initial Network followed by canary Standby) so consumers that classify
// or coordinate official relays cannot overlook the standby wave.
func (p Plan) DirectoryCandidates() []Candidate {
	return slices.DeleteFunc(p.NetworkCandidates(), func(c Candidate) bool { return c.Source != Official })
}
func (p Plan) NetworkCandidates() []Candidate { return slices.Concat(p.Network, p.Standby) }

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
		plan.Configured = append(plan.Configured, Candidate{URL: raw, Identity: endpoint.IdentityURL, Source: Configured})
	}
	available := min(MaximumNetworkCandidates, MaximumEndpoints-len(plan.Configured))
	if input.Policy.ConfiguredOnly || available == 0 {
		return finish(plan), nil
	}
	order := newSelectionOrder(input)
	var ready, cooled []Candidate
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
		}
		// A current directory identity keeps its source even when its role is disabled.
		seen[endpoint.IdentityURL] = true
		if reason != "" {
			plan.Decision.Filters = append(plan.Decision.Filters, Filter{Endpoint: node.Relay, Reason: reason})
			continue
		}
		candidate := Candidate{Source: Official, URL: node.Relay, Identity: endpoint.IdentityURL, Alias: node.Alias, Zone: node.Zone,
			Region: node.Region, Provider: node.Provider, Operator: node.Operator, CapacityMbps: node.CapacityMbps, Canary: node.Status == nodedirectory.Canary}
		fact := order.applyHistory(&candidate)
		if !fact.CooldownUntil.IsZero() || !fact.ServerRetryAt.IsZero() {
			candidate.CooldownUntil, candidate.ServerRetryAt = fact.CooldownUntil, fact.ServerRetryAt
			cooled = append(cooled, candidate)
		} else {
			ready = append(ready, candidate)
		}
	}
	order.selectNetwork(&plan, available, ready, cooled)
	plan.addCommunity(input.Community, available, seen, order)
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

func BackupIndex(primary Candidate, candidates []Candidate, policy Policy) int {
	return backupIndex(primary, candidates, policy, nil)
}

func backupIndex(primary Candidate, candidates []Candidate, policy Policy, eligible func(Candidate) bool) int {
	// A healthy or unmeasured backup outranks a recent failure even if that
	// failure has a better zone. Admission reuses the same decision after racing.
	const (
		otherOperator = 1 << iota
		otherProvider
		pairZone
		notCanary
		withoutFailure
	)
	best, score := 0, -1
	preferredZone := "global"
	if primary.Zone == "global" && !policy.GlobalPairCoversMainland {
		preferredZone = "cn"
	}
	for index, candidate := range candidates {
		if eligible != nil && !eligible(candidate) {
			continue
		}
		value := 0
		if candidate.history != recentlyFailed {
			value += withoutFailure
		}
		if !candidate.Canary {
			value += notCanary
		}
		if candidate.Zone == preferredZone {
			value += pairZone
		}
		if candidate.Provider != "" && candidate.Provider != primary.Provider {
			value += otherProvider
		}
		if candidate.Operator != "" && candidate.Operator != primary.Operator {
			value += otherOperator
		}
		if value > score {
			best, score = index, value
		}
	}
	return best
}
