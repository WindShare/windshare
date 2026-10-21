package turnpath

import "testing"

func TestMilestoneContractIsCompleteAndClosed(t *testing.T) {
	seen := make(map[string]bool)
	for _, milestone := range Milestones() {
		name, valid := milestone.Name()
		if !valid || seen[name] || milestone.String() != name {
			t.Fatalf("missing or ambiguous milestone %d: %q", milestone, name)
		}
		seen[name] = true
	}
	for _, invalid := range []Milestone{0, milestoneLimit, 255} {
		if name, valid := invalid.Name(); valid || name != "" {
			t.Fatalf("unknown milestone accepted: %d", invalid)
		}
	}
	values := Milestones()
	values[0] = 0
	if Milestones()[0] == 0 {
		t.Fatal("consumer mutated the source vocabulary")
	}
}
