package relayset

import (
	"context"
	"slices"
	"testing"

	"github.com/windshare/windshare/connectivity/relayplan"
)

func TestCommunityAdmissionReservesBothOfficialSlots(t *testing.T) {
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "cn", Source: relayplan.Official, Zone: "cn"}, {URL: "global", Source: relayplan.Official, Zone: "global"}, {URL: "community", Source: relayplan.Community, Verified: true}}}
	a := NewNetworkAdmissions(plan, nil, nil)
	var admitted []Admission
	a.Observe(func(event Admission) { admitted = append(admitted, event) })
	for _, url := range []string{"community", "global", "cn"} {
		permit, err := a.Control(url).Begin(t.Context())
		if err != nil || !permit.Admit() {
			t.Fatal(url, err)
		}
		permit.Release()
	}
	if len(admitted) != 3 || admitted[0].Primary != "" || admitted[1].Primary != "global" || admitted[2].Reason != AdmissionReasonBackup {
		t.Fatal(admitted)
	}
}

func TestCommunityPublishesDuringOfficialAccountWait(t *testing.T) {
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "cn", Source: relayplan.Official, Zone: "cn"}, {URL: "global", Source: relayplan.Official, Zone: "global"}}, Standby: []relayplan.Candidate{{URL: "community", Source: relayplan.Community}}}
	pending := map[string]*waitingRegistration{}
	for _, c := range plan.Network {
		pending[c.URL] = &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	}
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		if p := pending[url]; p != nil {
			return p, nil
		}
		return newEndpoint(), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	for _, c := range plan.Network {
		waitValue(t, pending[c.URL].observed)
		pending[c.URL].publish(RegistrationWaitingAuthorization)
	}
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if set.ReadyRelayURL() != "community" || set.Status().AuthorizationRequired || !slices.Equal(set.Registrations().Candidates, []string{"cn", "community", "global"}) {
		t.Fatal(set.Status(), set.Registrations())
	}
}
