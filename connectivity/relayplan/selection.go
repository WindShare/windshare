package relayplan

import (
	"hash/fnv"
	"slices"
	"sort"
	"time"
)

const (
	BulkCapacityThresholdMbps uint32 = 50
	DurationBucket                   = 100 * time.Millisecond
)

type selectionOrder struct {
	facts      map[string]Observation
	seedPrefix string
}

func newSelectionOrder(input Input) selectionOrder {
	order := selectionOrder{facts: make(map[string]Observation)}
	if input.SelectionSeed != "" {
		order.seedPrefix = input.SelectionSeed + "|" + input.NetworkGeneration + "|"
	}
	for _, fact := range input.Observations {
		if fact.NetworkGeneration != input.NetworkGeneration || fact.At.After(input.Now) || input.Now.Sub(fact.At) > ObservationAge {
			continue
		}
		if previous, ok := order.facts[fact.Identity]; !ok || fact.At.After(previous.At) {
			order.facts[fact.Identity] = fact.ActiveAt(input.Now)
		}
	}
	return order
}

func (o selectionOrder) less(a, b Candidate) bool {
	if a.history != b.history {
		return a.history > b.history
	}
	if a.Canary != b.Canary {
		return !a.Canary
	}
	if l, r := a.CapacityMbps >= BulkCapacityThresholdMbps, b.CapacityMbps >= BulkCapacityThresholdMbps; l != r {
		return l
	}
	if left, right := o.facts[a.Identity], o.facts[b.Identity]; left.Admitted && right.Admitted {
		if l, r := max(left.Duration, 0)/DurationBucket, max(right.Duration, 0)/DurationBucket; l != r {
			return l < r
		}
	}
	if a.CapacityMbps != b.CapacityMbps {
		return a.CapacityMbps > b.CapacityMbps
	}
	if o.seedPrefix != "" {
		hash := func(identity string) uint32 {
			h := fnv.New32a()
			_, _ = h.Write([]byte(o.seedPrefix + identity))
			return h.Sum32()
		}
		if l, r := hash(a.Identity), hash(b.Identity); l != r {
			return l < r
		}
	}
	return a.Identity < b.Identity
}

// A diversity reservation may break a tie within one retry priority. It must
// never displace a dialable fallback in favor of a server that asked us to wait.
func (o selectionOrder) retryPriority(a, b Candidate) int {
	left, right := !a.ServerRetryAt.IsZero(), !b.ServerRetryAt.IsZero()
	switch {
	case left && !right:
		return 1
	case !left && right:
		return -1
	case left && !a.ServerRetryAt.Equal(b.ServerRetryAt):
		return a.ServerRetryAt.Compare(b.ServerRetryAt)
	}
	l := o.facts[a.Identity].CooldownReason == CooldownNodeRejected
	r := o.facts[b.Identity].CooldownReason == CooldownNodeRejected
	if l != r {
		if l {
			return 1
		}
		return -1
	}
	return a.CooldownUntil.Compare(b.CooldownUntil)
}

func (o selectionOrder) selectNetwork(plan *Plan, available int, ready, cooled []Candidate) {
	sort.Slice(ready, func(i, j int) bool { return o.less(ready[i], ready[j]) })
	sort.Slice(cooled, func(i, j int) bool {
		if cmp := o.retryPriority(cooled[i], cooled[j]); cmp != 0 {
			return cmp < 0
		}
		return o.less(cooled[i], cooled[j])
	})
	target := min(TargetNetworkRegistrations, available)
	var active, canaries []Candidate
	for _, candidate := range ready {
		if candidate.Canary {
			canaries = append(canaries, candidate)
		} else {
			active = append(active, candidate)
		}
	}
	plan.Network = selectReady(active, target, plan.Policy)
	if len(active) > target {
		plan.Standby = append(plan.Standby, activeAlternates(active, plan.Network, target)...)
	}
	switch {
	case len(active) >= target:
		// A separate startup wave can reuse link slots if the active relays all fail.
		plan.Standby = append(plan.Standby, selectReady(canaries, target, plan.Policy)...)
	case len(active) == 0:
		plan.Network = selectReady(canaries, target, plan.Policy)
	case len(canaries) != 0:
		index := BackupIndex(active[0], canaries, plan.Policy)
		plan.Network = append(plan.Network, canaries[index])
	}
	if len(ready) == 0 && len(cooled) > 0 {
		plan.Decision.CooldownFallback = true
		plan.Network = append(plan.Network, cooled[0])
		cooled = cooled[1:]
	}
	if len(plan.Network) > 0 && len(plan.Network) < target && len(cooled) > 0 {
		index := o.cooledBackup(plan.Network[0], cooled, plan.Policy, plan.Decision.CooldownFallback)
		plan.Network = append(plan.Network, cooled[index])
		cooled = slices.Delete(cooled, index, index+1)
	}
	if plan.Decision.CooldownFallback {
		n := min(len(cooled), available-len(plan.Network))
		plan.Network = append(plan.Network, cooled[:n]...)
		// Remaining fallback nodes exceed the link limit. None were filtered for cooldown.
		return
	}
	for _, candidate := range cooled {
		plan.Decision.Filters = append(plan.Decision.Filters, Filter{Endpoint: candidate.URL, Reason: "cooldown"})
	}
}

// Local expiry jitter of less than one retry second does not justify losing
// an independent route. In all-soft fallback those local deadlines do not gate any dial.
const backupExpiryTolerance = time.Second

func (o selectionOrder) cooledBackup(primary Candidate, cooled []Candidate, policy Policy, fallback bool) int {
	if len(cooled) == 1 {
		return 0
	}
	first := cooled[0]
	rejected := o.facts[first.Identity].CooldownReason == CooldownNodeRejected
	return backupIndex(primary, cooled, policy, func(candidate Candidate) bool {
		return candidate.ServerRetryAt.IsZero() == first.ServerRetryAt.IsZero() &&
			(o.facts[candidate.Identity].CooldownReason == CooldownNodeRejected) == rejected &&
			candidate.ServerRetryAt.Sub(first.ServerRetryAt) <= backupExpiryTolerance &&
			(fallback || candidate.CooldownUntil.Sub(first.CooldownUntil) <= backupExpiryTolerance)
	})
}

func selectReady(ready []Candidate, available int, policy Policy) []Candidate {
	if len(ready) <= 1 {
		return ready
	}
	if available == 1 {
		return slices.Clone(ready[:1])
	}
	primary, rest := ready[0], ready[1:]
	backup := BackupIndex(primary, rest, policy)
	selected := []Candidate{primary, rest[backup]}
	rest = slices.Delete(slices.Clone(rest), backup, backup+1)
	if len(selected) < available {
		// Keep exploring an unmeasured endpoint even when known fast ones fill the prefix.
		if index := slices.IndexFunc(rest, func(c Candidate) bool { return c.history == unmeasured }); index >= 0 {
			selected = append(selected, rest[index])
			rest = slices.Delete(rest, index, index+1)
		}
	}
	return append(selected, rest[:min(len(rest), available-len(selected))]...)
}
