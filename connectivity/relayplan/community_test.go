package relayplan

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
)

func TestCommunitySupplementAndFallback(t *testing.T) {
	peer := func(host string, verified bool) Candidate {
		return Candidate{URL: "https://" + host, Source: Community, Verified: verified, Zone: "cn", Alias: "pretend", CapacityMbps: 10000}
	}
	cold, warm, ip := peer("cold.example", false), peer("warm.example", true), peer("1.1.1.1", true)
	in := Input{Now: time.Now(), Directory: nodedirectory.Payload{Nodes: []nodedirectory.Node{planNode(0, "global"), planNode(1, "cn"), planNode(2, "global")}}, Community: []Candidate{ip, cold, warm}}
	p := selected(t, in)
	if len(p.Network) != 3 || p.Network[0].Source != Official || p.Network[1].Source != Official || p.Network[0].Zone == p.Network[1].Zone || p.Network[2].URL != warm.URL || p.Network[2].Alias != "" || p.Network[2].Zone != "" {
		t.Fatal(p.Network)
	}
	if !p.Standby[len(p.Standby)-1].IPLiteral || len(p.DirectoryCandidates()) != 3 {
		t.Fatal(p.Standby)
	}
	in.Community = []Candidate{cold, ip}
	p = selected(t, in)
	if len(p.Network) != 2 {
		t.Fatal(p.Network)
	}
	in.Directory = nodedirectory.Payload{}
	p = selected(t, in)
	if len(p.Network) != 1 || p.Network[0].URL != cold.URL || !p.Standby[0].IPLiteral {
		t.Fatal(p)
	}
	in.Configured = []string{cold.URL}
	p = selected(t, in)
	if slices.ContainsFunc(p.NetworkCandidates(), func(c Candidate) bool { return c.URL == cold.URL }) {
		t.Fatal(p)
	}
}

func TestCommunityFallbackRunsBeforeOfficialServerDeadlines(t *testing.T) {
	now := time.Now()
	peer := Candidate{URL: "https://8.8.8.8", Source: Community}
	in := Input{Now: now, NetworkGeneration: "wifi", Community: []Candidate{peer}}
	for i := range MaximumNetworkCandidates {
		node := planNode(i, "global")
		in.Directory.Nodes = append(in.Directory.Nodes, node)
		ep, err := networkendpoint.NormalizeRelay(node.Relay)
		if err != nil {
			t.Fatal(err)
		}
		in.Observations = append(in.Observations, Observation{At: now, NetworkGeneration: "wifi", Identity: ep.IdentityURL, ServerRetryAt: now.Add(time.Minute)})
	}
	plan := selected(t, in)
	if !plan.Decision.CooldownFallback || len(plan.Network) != 1 || plan.Network[0].URL != peer.URL || len(plan.Standby) != MaximumNetworkCandidates {
		t.Fatal(plan)
	}
}

func communityObservation(t *testing.T, input Input, rawURL string) Observation {
	t.Helper()
	ep, err := networkendpoint.NormalizeRelay(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return Observation{Identity: ep.IdentityURL, At: input.Now, NetworkGeneration: input.NetworkGeneration}
}

func TestCommunityRankingOrdersHistoryAndClampsDuration(t *testing.T) {
	now := time.Now()
	peer := func(host string, verified bool, capacity uint32) Candidate {
		return Candidate{URL: "https://" + host, Source: Community, Verified: verified, CapacityMbps: capacity, ObservedDuration: time.Hour}
	}

	// A recently failed high-capacity peer must rank behind a healthy unmeasured peer,
	// while admitted peers rank first and expose their observation state and clamped duration.
	failed := peer("a-failed.example", true, 10000)
	unmeasured := peer("b-unmeasured.example", true, 25)
	admitted := peer("c-admitted.example", true, 25)
	in := Input{Now: now, NetworkGeneration: "1", Community: []Candidate{failed, unmeasured, admitted}}
	failFact := communityObservation(t, in, failed.URL)
	failFact.Duration = 500 * time.Millisecond
	okFact := communityObservation(t, in, admitted.URL)
	okFact.Admitted = true
	okFact.Duration = 145 * time.Millisecond
	in.Observations = []Observation{failFact, okFact}

	plan := selected(t, in)
	if len(plan.Network) != 2 || plan.Network[0].URL != admitted.URL || plan.Network[1].URL != unmeasured.URL {
		t.Fatalf("community history order = %+v, want [%s, %s]", plan.Network, admitted.URL, unmeasured.URL)
	}
	if d, ok := plan.Network[0].ObservedHandshake(); !ok || d != 145*time.Millisecond || plan.Network[0].HistoryState() != "admitted" {
		t.Fatalf("admitted community candidate = %+v (%v, %t)", plan.Network[0], d, ok)
	}
	if _, ok := plan.Network[1].ObservedHandshake(); ok || plan.Network[1].HistoryState() != "unmeasured" {
		t.Fatalf("unmeasured community candidate exposed handshake: %+v", plan.Network[1])
	}

	// Expired cooldown still leaves Admitted=false within ObservationAge; keep demoted behind unmeasured.
	failFact.CooldownUntil = now.Add(-time.Second)
	failFact.CooldownReason = CooldownServerBackoff
	in.Community = []Candidate{failed, unmeasured}
	in.Observations = []Observation{failFact}
	plan = selected(t, in)
	if len(plan.Network) != 2 || plan.Network[0].URL != unmeasured.URL || plan.Network[1].URL != failed.URL {
		t.Fatalf("expired-cooldown demotion = %+v, want [%s, %s]", plan.Network, unmeasured.URL, failed.URL)
	}
	if _, ok := plan.Network[1].ObservedHandshake(); ok || plan.Network[1].HistoryState() != "failed" {
		t.Fatalf("failed community candidate = %+v", plan.Network[1])
	}

	// Negative duration clamps to bucket 0 so higher capacity wins the same bucket; faster bucket beats capacity.
	slow, fast := peer("a-slow.example", true, 200), peer("b-fast.example", true, 100)
	in = Input{Now: now, NetworkGeneration: "1", Community: []Candidate{slow, fast}}
	slowFact, fastFact := communityObservation(t, in, slow.URL), communityObservation(t, in, fast.URL)
	slowFact.Admitted, fastFact.Admitted = true, true
	for _, tc := range []struct {
		slowDur, fastDur time.Duration
		winner           string
		wantDur          time.Duration
	}{
		{950 * time.Millisecond, 130 * time.Millisecond, fast.URL, 130 * time.Millisecond},
		{90 * time.Millisecond, -time.Second, slow.URL, 90 * time.Millisecond},
		{-time.Second, 90 * time.Millisecond, slow.URL, 0},
	} {
		slowFact.Duration, fastFact.Duration = tc.slowDur, tc.fastDur
		in.Observations = []Observation{slowFact, fastFact}
		got := selected(t, in).Network[0]
		if got.URL != tc.winner {
			t.Fatalf("bucket test %+v picked %s, want %s", tc, got.URL, tc.winner)
		}
		if d, ok := got.ObservedHandshake(); !ok || d != tc.wantDur {
			t.Fatalf("bucket test %+v observed duration = (%v, %t), want (%v, true)", tc, d, ok, tc.wantDur)
		}
	}

	// Verified must remain ahead of history so a single transient failure on the sole verified peer
	// does not let an unverified peer displace the supplemental verified slot.
	verifiedFailed := peer("z-verified.example", true, 25)
	unverified := peer("a-unverified.example", false, 1000)
	in = Input{
		Now:               now,
		NetworkGeneration: "1",
		Directory:         nodedirectory.Payload{Nodes: []nodedirectory.Node{planNode(0, "global"), planNode(1, "cn")}},
		Community:         []Candidate{unverified, verifiedFailed},
		Observations:      []Observation{communityObservation(t, Input{Now: now, NetworkGeneration: "1"}, verifiedFailed.URL)},
	}
	plan = selected(t, in)
	if len(plan.Network) != 3 || plan.Network[2].URL != verifiedFailed.URL || plan.Network[2].HistoryState() != "failed" {
		t.Fatalf("verified supplemental slot = %+v", plan.Network)
	}
}

func TestCommunityColdStartSeedSpreadsEqualCapacityPeers(t *testing.T) {
	now := time.Now()
	peers := make([]Candidate, 0, 12)
	for i := range 12 {
		peers = append(peers, Candidate{
			URL:          fmt.Sprintf("https://peer%02d.example", i),
			Source:       Community,
			Verified:     true,
			CapacityMbps: 25,
		})
	}

	// Empty seed preserves deterministic Identity order.
	base := Input{Now: now, NetworkGeneration: "1", Community: slices.Clone(peers)}
	if got := selected(t, base).Network[0].URL; got != peers[0].URL {
		t.Fatalf("empty seed picked %q, want %q", got, peers[0].URL)
	}

	// Fallback mode: distinct seeds spread selections across peers and ignore input order.
	fallbackVisited := map[string]bool{}
	for i := range 16 {
		in := Input{Now: now, NetworkGeneration: "1", SelectionSeed: fmt.Sprint(i), Community: slices.Clone(peers)}
		plan := selected(t, in)
		slices.Reverse(in.Community)
		if !slices.Equal(plan.Decision.Candidates, selected(t, in).Decision.Candidates) {
			t.Fatalf("fallback tie depended on input order for seed %d", i)
		}
		for _, c := range plan.Network {
			fallbackVisited[c.Identity] = true
		}
	}
	if len(fallbackVisited) <= TargetNetworkRegistrations {
		t.Fatalf("seed failed to spread fallback community picks: %v", fallbackVisited)
	}

	// Supplemental slot alongside healthy official pair also spreads across verified peers.
	supplementalVisited := map[string]bool{}
	dir := nodedirectory.Payload{Nodes: []nodedirectory.Node{planNode(0, "global"), planNode(1, "cn")}}
	for i := range 16 {
		in := Input{Now: now, NetworkGeneration: "1", SelectionSeed: fmt.Sprint(i), Directory: dir, Community: slices.Clone(peers)}
		plan := selected(t, in)
		if len(plan.Network) != 3 || plan.Network[2].Source != Community {
			t.Fatalf("unexpected supplemental plan: %+v", plan.Network)
		}
		supplementalVisited[plan.Network[2].Identity] = true
	}
	if len(supplementalVisited) < 2 {
		t.Fatalf("seed failed to spread supplemental community pick: %v", supplementalVisited)
	}
}

func TestCommunityProbeCooldownDemotionAndSingleNodeFallback(t *testing.T) {
	now := time.Now()
	sybil1 := Candidate{URL: "https://sybil1.example", Source: Community, CapacityMbps: ^uint32(0), CooldownUntil: now.Add(15 * time.Second), ProbeFailed: true}
	sybil2 := Candidate{URL: "https://sybil2.example", Source: Community, CapacityMbps: ^uint32(0), CooldownUntil: now.Add(30 * time.Second), ProbeFailed: true}
	good1 := Candidate{URL: "https://good1.example", Source: Community, CapacityMbps: 25}
	good2 := Candidate{URL: "https://good2.example", Source: Community, CapacityMbps: 25}

	// Active probe cooldown filters broken/Sybil high-capacity peers and surfaces healthy peers.
	plan := selected(t, Input{Now: now, NetworkGeneration: "1", Community: []Candidate{sybil1, sybil2, good1, good2}})
	if plan.Decision.CooldownFallback || len(plan.Network) != 2 || plan.Network[0].URL != good1.URL || plan.Network[1].URL != good2.URL {
		t.Fatalf("cooled sybils displaced healthy peers: %+v", plan)
	}
	if len(plan.Decision.Filters) != 2 || plan.Decision.Filters[0].Reason != "community_cooldown" || plan.Decision.Filters[1].Reason != "community_cooldown" {
		t.Fatalf("expected community_cooldown filters, got %+v", plan.Decision.Filters)
	}

	// After CooldownUntil expires, ProbeFailed keeps the high-capacity peer behind unmeasured peers
	// and reports HistoryState() == "failed".
	expired := sybil1
	expired.CooldownUntil = now.Add(-time.Second)
	plan = selected(t, Input{Now: now, NetworkGeneration: "1", Community: []Candidate{expired, good1, good2}})
	if len(plan.Network) != 2 || plan.Network[0].URL != good1.URL || plan.Network[1].URL != good2.URL || len(plan.Decision.Filters) != 0 {
		t.Fatalf("expired-cooldown ProbeFailed peer jumped ahead of unmeasured peers: %+v", plan)
	}
	plan = selected(t, Input{Now: now, NetworkGeneration: "1", Community: []Candidate{expired, good1}})
	if len(plan.Network) != 2 || plan.Network[0].URL != good1.URL || plan.Network[1].URL != expired.URL || plan.Network[1].HistoryState() != "failed" {
		t.Fatalf("ProbeFailed history projection = %+v", plan.Network)
	}

	// When NO configured, official, or uncooled community route exists, fall back to the
	// earliest-expiring cooled community peer instead of returning an empty plan.
	ipCooled := Candidate{URL: "https://1.1.1.1", Source: Community, CooldownUntil: now.Add(5 * time.Second), ProbeFailed: true}
	plan = selected(t, Input{Now: now, NetworkGeneration: "1", Community: []Candidate{sybil2, sybil1, ipCooled}})
	if !plan.Decision.CooldownFallback || len(plan.Decision.Filters) != 0 ||
		len(plan.Network) != 2 || plan.Network[0].URL != sybil1.URL || plan.Network[1].URL != sybil2.URL ||
		len(plan.Standby) != 1 || plan.Standby[0].URL != ipCooled.URL {
		t.Fatalf("sole cooled community fallback = %+v", plan)
	}

	// When an official or configured route exists, cooled community peers stay filtered.
	plan = selected(t, Input{Now: now, NetworkGeneration: "1", Configured: []string{"https://own.example"}, Community: []Candidate{sybil1}})
	if plan.Decision.CooldownFallback || len(plan.Network) != 0 || len(plan.Decision.Filters) != 1 || plan.Decision.Filters[0].Reason != "community_cooldown" {
		t.Fatalf("configured plan unexpectedly fell back to cooled community peer: %+v", plan)
	}
}
