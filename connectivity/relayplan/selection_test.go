package relayplan

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
)

func observed(t *testing.T, input Input, node nodedirectory.Node) Observation {
	t.Helper()
	endpoint, err := networkendpoint.NormalizeRelay(node.Relay)
	if err != nil {
		t.Fatal(err)
	}
	return Observation{Identity: endpoint.IdentityURL, At: input.Now, NetworkGeneration: input.NetworkGeneration}
}

func selected(t *testing.T, input Input) Plan {
	t.Helper()
	plan, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestCooldownReservesOnlyMissingSlots(t *testing.T) {
	input := Input{Now: time.Now(), NetworkGeneration: "7"}
	for i := range 5 {
		input.Directory.Nodes = append(input.Directory.Nodes, planNode(i, "global"))
	}
	nodes := input.Directory.Nodes
	// Node 1 would win the diversity reservation, but may not be dialed yet.
	nodes[1].Zone = "cn"
	for _, node := range nodes[1:] {
		fact := observed(t, input, node)
		fact.CooldownReason = CooldownTransportFailure
		fact.CooldownUntil = input.Now.Add(time.Minute)
		input.Observations = append(input.Observations, fact)
	}
	input.Observations[0].ServerRetryAt = input.Now.Add(10 * time.Second)
	input.Observations[1].CooldownReason = CooldownNodeRejected
	input.Observations[2].CooldownUntil = input.Now.Add(15 * time.Second)
	plan := selected(t, input)
	if plan.Decision.CooldownFallback || len(plan.Network) != 2 || plan.Network[0].URL != nodes[0].Relay || plan.Network[1].URL != nodes[3].Relay || len(plan.Decision.Filters) != 3 {
		t.Fatal("ready plus earliest dialable transient backup", plan)
	}
	for _, f := range plan.Decision.Filters {
		if f.Reason != "cooldown" || slices.Contains(plan.Decision.Candidates, f.Endpoint) {
			t.Fatal(f)
		}
	}
	input.Observations[2].CooldownUntil = input.Now
	input.Observations[2].Admitted = true
	plan = selected(t, input)
	if len(plan.Network) != 2 || len(plan.Decision.Filters) != 3 || !plan.Network[1].CooldownUntil.IsZero() {
		t.Fatal("sufficient ready nodes", plan)
	}
	for i := range MaximumEndpoints - 1 {
		input.Configured = append(input.Configured, fmt.Sprintf("https://private%d.example", i))
	}
	plan = selected(t, input)
	if len(plan.Network) != 1 || plan.Network[0].URL != nodes[3].Relay || len(plan.Configured) != MaximumEndpoints-1 {
		t.Fatal("shared slots", plan)
	}
}

func TestAllCooledRespectsServerOrderBeforeDiversity(t *testing.T) {
	input := Input{Now: time.Now(), NetworkGeneration: "7"}
	for i := range 6 {
		node := planNode(i, "global")
		if i == 5 {
			node.Zone = "cn"
		}
		input.Directory.Nodes = append(input.Directory.Nodes, node)
		fact := observed(t, input, node)
		fact.CooldownUntil = input.Now.Add(time.Minute)
		fact.ServerRetryAt = input.Now.Add(time.Duration(i+1) * time.Second)
		input.Observations = append(input.Observations, fact)
	}
	check := func(want ...int) {
		t.Helper()
		plan := selected(t, input)
		if !plan.Decision.CooldownFallback || len(plan.Decision.Filters) != 0 || len(plan.Network) != len(want) {
			t.Fatal(plan)
		}
		for i, n := range want {
			if plan.Network[i].URL != input.Directory.Nodes[n].Relay {
				t.Fatal(want, plan.Network)
			}
		}
	}
	check(0, 1, 2, 3)
	input.Observations[4].ServerRetryAt = time.Time{}
	input.Observations[3].ServerRetryAt = time.Time{}
	input.Observations[3].CooldownReason = CooldownNodeRejected
	check(4, 3, 0, 1)
}

func TestSelectionOrdersCapacityAndHandshakeBuckets(t *testing.T) {
	input := Input{Now: time.Now(), NetworkGeneration: "9"}
	a, b := planNode(0, "global"), planNode(1, "global")
	a.CapacityMbps, b.CapacityMbps = 200, 100
	input.Directory.Nodes = []nodedirectory.Node{a, b}
	input.Observations = []Observation{observed(t, input, a), observed(t, input, b)}
	for i := range input.Observations {
		input.Observations[i].Admitted = true
	}
	for _, test := range []struct {
		a, b     time.Duration
		capacity uint32
		winner   int
	}{
		{950 * time.Millisecond, 130 * time.Millisecond, 100, 1},
		{140 * time.Millisecond, 110 * time.Millisecond, 100, 0},
		{950 * time.Millisecond, time.Millisecond, 49, 0},
		{950 * time.Millisecond, time.Millisecond, 50, 1},
		{-time.Second, 90 * time.Millisecond, 200, 0},
	} {
		input.Observations[0].Duration, input.Observations[1].Duration = test.a, test.b
		input.Directory.Nodes[1].CapacityMbps = test.capacity
		if got := selected(t, input).Network[0]; got.URL != input.Directory.Nodes[test.winner].Relay {
			t.Fatal(test, got)
		}
	}
	input.Directory.Nodes[1].Status = nodedirectory.Canary
	input.Observations[0].Duration = time.Second
	if selected(t, input).Network[0].URL != a.Relay {
		t.Fatal("canary bypassed active")
	}
	input.Observations[0].Admitted = false
	if selected(t, input).Network[0].URL != b.Relay {
		t.Fatal("recent failure bypassed successful canary")
	}
}

func TestFailedDiverseCandidateCanRaceWithoutReservingBackup(t *testing.T) {
	input := Input{Now: time.Now()}
	for i := range 5 {
		input.Directory.Nodes = append(input.Directory.Nodes, planNode(i, "global"))
	}
	nodes := input.Directory.Nodes
	nodes[1].Zone = "cn"
	input.Observations = []Observation{observed(t, input, nodes[0]), observed(t, input, nodes[1]), observed(t, input, nodes[2]), observed(t, input, nodes[3])}
	for _, i := range []int{0, 2, 3} {
		input.Observations[i].Admitted = true
	}
	plan := selected(t, input)
	if plan.Network[1].URL == nodes[1].Relay || plan.Network[2].URL != nodes[4].Relay {
		t.Fatal(plan.Network)
	}
	// With room for every endpoint, the admission coordinator still uses the
	// same health preference when a different candidate wins the actual race.
	input.Directory.Nodes = nodes[:4]
	plan = selected(t, input)
	if backup := BackupIndex(plan.Network[1], plan.Network[2:], plan.Policy); plan.Network[2+backup].URL == nodes[1].Relay {
		t.Fatal(plan.Network)
	}
}

func TestColdStartSeedIsStableAndExploresBeyondTheLimit(t *testing.T) {
	input := Input{Now: time.Now(), NetworkGeneration: "1"}
	for i := range 12 {
		input.Directory.Nodes = append(input.Directory.Nodes, planNode(i, "global"))
	}
	initial := selected(t, input)
	if initial.Network[0].Alias != "node0" {
		t.Fatal("empty seed lost identity order", initial.Network)
	}
	visited := map[string]bool{}
	for i := range 16 {
		input.SelectionSeed = fmt.Sprint(i)
		plan := selected(t, input)
		slices.Reverse(input.Directory.Nodes)
		if !slices.Equal(plan.Decision.Candidates, selected(t, input).Decision.Candidates) {
			t.Fatal("tie depended on directory order", i)
		}
		for _, c := range plan.Network {
			visited[c.Identity] = true
		}
	}
	if len(visited) <= MaximumNetworkCandidates {
		t.Fatal("seed failed to spread candidate sets", visited)
	}
}

func TestCooledBackupPreservesDiversityAcrossStaggeredFailures(t *testing.T) {
	for _, mode := range []string{"one ready", "all soft", "all hard"} {
		t.Run(mode, func(t *testing.T) {
			input := Input{Now: time.Now(), NetworkGeneration: "1"}
			for i := range MaximumNetworkCandidates + 1 {
				node := planNode(i, "global")
				if i == MaximumNetworkCandidates {
					node.Zone = "cn"
				}
				input.Directory.Nodes = append(input.Directory.Nodes, node)
				if mode == "one ready" && i == 0 {
					continue
				}
				fact := observed(t, input, node)
				fact.CooldownUntil = input.Now.Add(time.Minute + time.Duration(i)*time.Millisecond)
				fact.CooldownReason = CooldownTransportFailure
				if mode == "all hard" {
					fact.ServerRetryAt = fact.CooldownUntil
				}
				input.Observations = append(input.Observations, fact)
			}
			plan := selected(t, input)
			if len(plan.Network) < 2 || plan.Network[0].Alias != input.Directory.Nodes[0].Alias || plan.Network[1].Zone != "cn" {
				t.Fatal(plan.Network)
			}
		})
	}
}

func TestCandidateDeadlinesExpireIndependently(t *testing.T) {
	input := Input{Now: time.Now(), NetworkGeneration: "1"}
	for i := range 2 {
		node := planNode(i, "global")
		input.Directory.Nodes = append(input.Directory.Nodes, node)
		fact := observed(t, input, node)
		fact.CooldownUntil, fact.ServerRetryAt = input.Now, input.Now.Add(time.Minute)
		if i == 1 {
			fact.CooldownUntil, fact.ServerRetryAt = fact.ServerRetryAt, fact.CooldownUntil
		}
		input.Observations = append(input.Observations, fact)
	}
	candidates := selected(t, input).Network
	if len(candidates) != 2 || !candidates[0].ServerRetryAt.IsZero() || !candidates[0].CooldownUntil.After(input.Now) ||
		!candidates[1].CooldownUntil.IsZero() || !candidates[1].ServerRetryAt.After(input.Now) {
		t.Fatal(candidates)
	}
}

func TestCandidateRetainsObservationHistoryAndHandshakeDuration(t *testing.T) {
	now := time.Now()
	admitted, failed, unmeasured := planNode(0, "global"), planNode(1, "global"), planNode(2, "global")
	input := Input{
		Now:               now,
		NetworkGeneration: "1",
		Directory:         nodedirectory.Payload{Nodes: []nodedirectory.Node{admitted, failed, unmeasured}},
	}
	okFact := observed(t, input, admitted)
	okFact.Admitted = true
	okFact.Duration = 0
	failFact := observed(t, input, failed)
	failFact.Duration = 500 * time.Millisecond
	input.Observations = []Observation{okFact, failFact}

	byAlias := map[string]Candidate{}
	for _, c := range selected(t, input).Network {
		byAlias[c.Alias] = c
	}
	if d, ok := byAlias[admitted.Alias].ObservedHandshake(); !ok || d != 0 || byAlias[admitted.Alias].HistoryState() != "admitted" {
		t.Fatalf("admitted candidate = %+v (%v, %t)", byAlias[admitted.Alias], d, ok)
	}
	if _, ok := byAlias[failed.Alias].ObservedHandshake(); ok || byAlias[failed.Alias].HistoryState() != "failed" {
		t.Fatalf("failed candidate exposed handshake duration: %+v", byAlias[failed.Alias])
	}
	if _, ok := byAlias[unmeasured.Alias].ObservedHandshake(); ok || byAlias[unmeasured.Alias].HistoryState() != "unmeasured" {
		t.Fatalf("unmeasured candidate exposed handshake duration: %+v", byAlias[unmeasured.Alias])
	}
}
