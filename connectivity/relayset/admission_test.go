package relayset

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

func admissionFixture() relayplan.Plan {
	return relayplan.Plan{Network: []relayplan.Candidate{
		{URL: "https://a.example", Zone: "global", Provider: "a", Operator: "one"},
		{URL: "https://b.example", Zone: "global", Provider: "b", Operator: "two"},
		{URL: "https://c.example", Zone: "cn", Provider: "c", Operator: "one"},
		{URL: "https://d.example", Zone: "global", Provider: "a", Operator: "one"},
	}}
}

func beginCandidates(t *testing.T, a *NetworkAdmissions, plan relayplan.Plan) []senderrelay.AttemptPermit {
	t.Helper()
	var result []senderrelay.AttemptPermit
	for _, candidate := range plan.Network {
		permit, err := a.Control(candidate.URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, permit)
	}
	return result
}

func TestAdmissionsUseActualPrimaryAndParkSurplus(t *testing.T) {
	plan := admissionFixture()
	a := NewNetworkAdmissions(plan)
	permits := beginCandidates(t, a, plan)
	if !permits[2].Admit() {
		t.Fatal("first successful candidate was not primary")
	}
	if permits[0].Admit() {
		t.Fatal("same-operator candidate displaced a more diverse backup")
	}
	permits[0].Release()
	if !permits[1].Admit() {
		t.Fatal("diverse global backup refused")
	}
	if permits[3].Admit() {
		t.Fatal("surplus candidate admitted")
	}
	for _, permit := range permits {
		permit.Release()
	}
	if a.active != 2 || a.inflight != 0 {
		t.Fatalf("active=%d inflight=%d", a.active, a.inflight)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Control(plan.Network[3].URL).Begin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("parked shutdown: %v", err)
	}
	a.Control(plan.Network[2].URL).Unavailable()
	if a.primary != 1 || a.active != 1 {
		t.Fatal("live backup did not retain admission")
	}
	resume, err := a.Control(plan.Network[2].URL).Begin(t.Context())
	if err != nil || !resume.Admit() {
		t.Fatalf("resume after loss: %v", err)
	}
	resume.Release()
	if a.active != 2 {
		t.Fatal("resume did not restore pair")
	}
}

func TestFailedPreferredBackupReleasesAttemptForFallback(t *testing.T) {
	plan := admissionFixture()
	a := NewNetworkAdmissions(plan)
	permits := beginCandidates(t, a, plan)
	if !permits[0].Admit() {
		t.Fatal("primary")
	}
	permits[0].Release()
	permits[2].Release()
	if a.inflight != 2 {
		t.Fatal("backoff retained dial slot")
	}
	if !permits[1].Admit() {
		t.Fatal("failed preferred zone prevented usable fallback")
	}
	if permits[3].Admit() {
		t.Fatal("third admission")
	}
	permits[1].Release()
	permits[3].Release()
	if a.Control("https://private.example") != nil {
		t.Fatal("configured endpoint was gated")
	}
}
