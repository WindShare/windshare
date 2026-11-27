package relayplan

import (
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
