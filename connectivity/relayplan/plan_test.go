package relayplan

import (
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
)

func planNode(i int, zone string) nodedirectory.Node {
	return nodedirectory.Node{Alias: fmt.Sprintf("node%d", i), Relay: fmt.Sprintf("https://node%d.example", i), Roles: []string{"relay"}, Zone: zone,
		Provider: fmt.Sprint(i), Operator: fmt.Sprint(i), CapacityMbps: 50, Status: nodedirectory.Active}
}

func TestPlanReservesDiversityWithinSharedLimit(t *testing.T) {
	input := Input{Now: time.Now(), NetworkGeneration: "one"}
	for i := range 10 {
		input.Directory.Nodes = append(input.Directory.Nodes, planNode(i, "global"))
	}
	input.Directory.Nodes = append(input.Directory.Nodes, planNode(20, "cn"))
	plan, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Network) != 4 || plan.Network[1].Zone != "cn" {
		t.Fatalf("missing reserved backup: %+v", plan.Network)
	}
	for i := range 7 {
		input.Configured = append(input.Configured, fmt.Sprintf("https://private%d.example", i))
	}
	plan, err = Build(input)
	if err != nil || len(plan.Network) != 1 || len(plan.Configured) != 7 {
		t.Fatalf("shared limit: %+v %v", plan, err)
	}
	input.Policy.ConfiguredOnly = true
	plan, err = Build(input)
	if err != nil || len(plan.Network) != 0 {
		t.Fatal("configured-only added infrastructure")
	}
}

func TestPlanFiltersTrustRolesAndExpiredObservations(t *testing.T) {
	now := time.Now()
	a, b, c, d := planNode(1, "global"), planNode(2, "global"), planNode(3, "cn"), planNode(4, "global")
	a.Status = nodedirectory.Draining
	c.Roles = []string{"stun"}
	input := Input{Now: now, NetworkGeneration: "new", Directory: nodedirectory.Payload{Nodes: []nodedirectory.Node{a, b, c, d}},
		Observations: []Observation{{Identity: b.Relay, NetworkGeneration: "old", At: now, CooldownUntil: now.Add(time.Hour)},
			{Identity: d.Relay, NetworkGeneration: "new", At: now, CooldownUntil: now.Add(time.Hour)}}}
	for i := range input.Observations {
		endpoint, _ := networkendpoint.NormalizeRelay(input.Observations[i].Identity)
		input.Observations[i].Identity = endpoint.IdentityURL
	}
	plan, err := Build(input)
	if err != nil || len(plan.Network) != 1 || plan.Network[0].Alias != b.Alias {
		t.Fatalf("filter result: %+v %v", plan, err)
	}
	if len(plan.Decision.Filters) != 3 {
		t.Fatal("missing rejection reasons")
	}
}
