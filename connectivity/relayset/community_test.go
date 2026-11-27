package relayset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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

func TestCommunityAuthBypassRotatesAcrossFailedStandbys(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn", Source: relayplan.Official, Zone: "cn"},
			{URL: "global", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "comm1", Source: relayplan.Community},
			{URL: "comm2", Source: relayplan.Community},
			{URL: "comm-ip", Source: relayplan.Community, IPLiteral: true},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"comm1":  {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	comm1Canceled := make(chan struct{})
	comm2Dialed := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "cn", "global":
			return pending[url], nil
		case "comm1":
			context.AfterFunc(ctx, func() { close(comm1Canceled) })
			return pending[url], nil
		case "comm2":
			close(comm2Dialed)
			return nil, errors.New("community check failed")
		case "comm-ip":
			return newEndpoint(), nil
		default:
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	for _, url := range []string{"cn", "global"} {
		waitValue(t, pending[url].observed)
		pending[url].publish(RegistrationWaitingAuthorization)
	}
	waitValue(t, pending["comm1"].observed)
	pending["comm1"].publish(RegistrationRetrying)
	waitValue(t, comm1Canceled)
	waitValue(t, comm2Dialed)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if set.ReadyRelayURL() != "comm-ip" || set.Status().AuthorizationRequired ||
		!slices.Equal(set.Registrations().Candidates, []string{"cn", "comm-ip", "global"}) ||
		!slices.Equal(set.Registrations().Rejected, []string{"comm2"}) {
		t.Fatal(set.Status(), set.Registrations())
	}
}

func TestNeedsUserAuthorizationPreservesOfficialRoutesAndRotatesCommunityStandbys(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn1", Source: relayplan.Official, Zone: "cn"},
			{URL: "global1", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "cn2", Source: relayplan.Official, Zone: "cn"},
			{URL: "cn-canary", Source: relayplan.Official, Zone: "cn", Canary: true},
			{URL: "comm1", Source: relayplan.Community},
			{URL: "comm2", Source: relayplan.Community},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn1":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global1": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	releaseComm1 := make(chan struct{})
	comm1Dialed := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "cn1", "global1":
			return pending[url], nil
		case "comm1":
			close(comm1Dialed)
			select {
			case <-releaseComm1:
				return nil, errors.New("community unreachable")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "comm2":
			return newEndpoint(), nil
		default:
			t.Errorf("unexpected dial during shared official auth wait: %s", url)
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	var mu sync.Mutex
	sawAuthRequired := false
	set.ObserveStatus(func(status SenderStatus) {
		mu.Lock()
		sawAuthRequired = sawAuthRequired || status.AuthorizationRequired
		mu.Unlock()
	})
	for _, url := range []string{"cn1", "global1"} {
		waitValue(t, pending[url].observed)
		pending[url].publish(RegistrationNeedsUserAuthorization)
	}
	waitValue(t, comm1Dialed)
	close(releaseComm1)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	premature := sawAuthRequired
	mu.Unlock()
	if premature {
		t.Fatal("AuthorizationRequired turned true while another community standby was available")
	}
	if set.ReadyRelayURL() != "comm2" || !slices.Equal(set.Registrations().Candidates, []string{"cn1", "comm2", "global1"}) {
		t.Fatal(set.ReadyRelayURL(), set.Registrations())
	}
}

func TestNeedsUserAuthorizationReportsRequiredOnlyAfterCommunityStandbysExhausted(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn1", Source: relayplan.Official, Zone: "cn"},
			{URL: "global1", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "comm1", Source: relayplan.Community},
			{URL: "comm2", Source: relayplan.Community},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn1":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global1": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"comm1":   {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	comm2Dialed := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "cn1", "global1", "comm1":
			return pending[url], nil
		case "comm2":
			close(comm2Dialed)
			return nil, errors.New("community rejected")
		default:
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	authRequired := make(chan struct{}, 1)
	set.ObserveStatus(func(status SenderStatus) {
		if status.AuthorizationRequired {
			select {
			case authRequired <- struct{}{}:
			default:
			}
		}
	})
	for _, url := range []string{"cn1", "global1"} {
		waitValue(t, pending[url].observed)
		pending[url].publish(RegistrationNeedsUserAuthorization)
	}
	waitValue(t, pending["comm1"].observed)
	if set.Status().AuthorizationRequired {
		t.Fatal("AuthorizationRequired turned true while comm1 was pending")
	}
	// A retrying first probe must be retired when replaced so it cannot suppress
	// AuthorizationRequired once the final community standby fails terminally.
	pending["comm1"].publish(RegistrationRetrying)
	waitValue(t, comm2Dialed)
	waitValue(t, authRequired)
	if !set.Status().AuthorizationRequired || !slices.Equal(set.Registrations().Candidates, []string{"cn1", "global1"}) {
		t.Fatal(set.Status(), set.Registrations())
	}
}

func TestCommunityStartsWhenSecondOfficialFailsAfterFirstWaitsForAuth(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn", Source: relayplan.Official, Zone: "cn"},
			{URL: "global", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "community", Source: relayplan.Community},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	communityDialed := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		if p := pending[url]; p != nil {
			return p, nil
		}
		close(communityDialed)
		return newEndpoint(), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, pending["cn"].observed)
	waitValue(t, pending["global"].observed)
	pending["cn"].publish(RegistrationWaitingAuthorization)
	select {
	case <-communityDialed:
		t.Fatal("community started while global official route was still pending")
	default:
	}
	pending["global"].publish(RegistrationRetrying)
	waitValue(t, communityDialed)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if set.ReadyRelayURL() != "community" || !slices.Equal(set.Registrations().Candidates, []string{"cn", "community", "global"}) {
		t.Fatal(set.ReadyRelayURL(), set.Registrations())
	}
}

func TestPrewarmedCommunityFailureDuringOfficialAuthWaitReplacesAtLimit(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn", Source: relayplan.Official, Zone: "cn"},
			{URL: "global", Source: relayplan.Official, Zone: "global"},
			{URL: "comm-warm", Source: relayplan.Community, Verified: true},
		},
		Standby: []relayplan.Candidate{
			{URL: "comm-standby", Source: relayplan.Community},
		},
	}
	// Five configured relays bound the network budget to 3 slots (exact len(Network)).
	for i := range MaximumEndpoints - len(plan.Network) {
		plan.Configured = append(plan.Configured, relayplan.Candidate{URL: fmt.Sprintf("own-%d", i), Source: relayplan.Configured})
	}
	pending := map[string]*waitingRegistration{
		"cn":        {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global":    {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"comm-warm": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	warmCanceled := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "cn", "global":
			return pending[url], nil
		case "comm-warm":
			context.AfterFunc(ctx, func() { close(warmCanceled) })
			return pending[url], nil
		case "comm-standby":
			return newEndpoint(), nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	for _, url := range []string{"cn", "global", "comm-warm"} {
		waitValue(t, pending[url].observed)
	}
	pending["cn"].publish(RegistrationWaitingAuthorization)
	pending["global"].publish(RegistrationWaitingAuthorization)
	pending["comm-warm"].publish(RegistrationRetrying)
	waitValue(t, warmCanceled)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidates := set.Registrations().Candidates
	if set.ReadyRelayURL() != "comm-standby" || len(candidates) != MaximumEndpoints ||
		slices.Contains(candidates, "comm-warm") || !slices.Contains(candidates, "comm-standby") {
		t.Fatal(set.ReadyRelayURL(), candidates)
	}
}

func TestOfficialFailureWithoutAuthWaitKeepsCanaryTierAheadOfCommunity(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn1", Source: relayplan.Official, Zone: "cn"},
			{URL: "global1", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "cn-canary", Source: relayplan.Official, Zone: "cn", Canary: true},
			{URL: "global-canary", Source: relayplan.Official, Zone: "global", Canary: true},
			{URL: "community", Source: relayplan.Community},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn1":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global1": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	canaryDialed := make(chan string, 2)
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "cn1", "global1":
			return pending[url], nil
		case "cn-canary", "global-canary":
			canaryDialed <- url
			return newEndpoint(), nil
		default:
			t.Errorf("unexpected dial ahead of official canary wave: %s", url)
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, pending["cn1"].observed)
	waitValue(t, pending["global1"].observed)
	// Single-route failure must not prematurely substitute an official canary.
	pending["cn1"].publish(RegistrationRetrying)
	select {
	case url := <-canaryDialed:
		t.Fatalf("prematurely promoted canary %s while global1 was still pending", url)
	default:
	}
	pending["global1"].publish(RegistrationRetrying)
	waitValue(t, canaryDialed)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if set.ReadyRelayURL() != "cn-canary" && set.ReadyRelayURL() != "global-canary" {
		t.Fatalf("ReadyRelayURL() = %q, want official canary", set.ReadyRelayURL())
	}
}

func TestPureCommunityPlanKeepsWholeWaveFallbackOrder(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "comm1", Source: relayplan.Community},
			{URL: "comm2", Source: relayplan.Community},
		},
		Standby: []relayplan.Candidate{
			{URL: "ip1", Source: relayplan.Community, IPLiteral: true},
			{URL: "ip2", Source: relayplan.Community, IPLiteral: true},
		},
	}
	pending := map[string]*waitingRegistration{
		"comm1": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"comm2": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	ipDialed := make(chan string, 2)
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		if p := pending[url]; p != nil {
			return p, nil
		}
		ipDialed <- url
		return newEndpoint(), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, pending["comm1"].observed)
	waitValue(t, pending["comm2"].observed)
	pending["comm1"].publish(RegistrationRetrying)
	select {
	case url := <-ipDialed:
		t.Fatalf("started IP-literal standby %s while comm2 was still pending", url)
	default:
	}
	pending["comm2"].publish(RegistrationRetrying)
	first, second := waitValue(t, ipDialed), waitValue(t, ipDialed)
	gotWave := []string{first, second}
	slices.Sort(gotWave)
	if !slices.Equal(gotWave, []string{"ip1", "ip2"}) {
		t.Fatalf("IP standby wave = %v, want [ip1 ip2]", gotWave)
	}
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialNonCanaryAlternateSubstitutesFailedZone(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn1", Source: relayplan.Official, Zone: "cn"},
			{URL: "global1", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "cn2", Source: relayplan.Official, Zone: "cn"},
			{URL: "cn-canary", Source: relayplan.Official, Zone: "cn", Canary: true},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn1":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global1": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	cn1Canceled := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "cn1":
			context.AfterFunc(ctx, func() { close(cn1Canceled) })
			return pending[url], nil
		case "global1":
			return pending[url], nil
		case "cn2":
			return newEndpoint(), nil
		default:
			t.Errorf("unexpected dial: %s", url)
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, pending["cn1"].observed)
	waitValue(t, pending["global1"].observed)
	pending["cn1"].publish(RegistrationRetrying)
	waitValue(t, cn1Canceled)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if set.ReadyRelayURL() != "cn2" || !slices.Equal(set.Registrations().Candidates, []string{"cn2", "global1"}) {
		t.Fatal(set.ReadyRelayURL(), set.Registrations())
	}
}

func TestCommunityAuthBypassRetiresFailedOfficialRouteAtLimit(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "cn1", Source: relayplan.Official, Zone: "cn"},
			{URL: "global1", Source: relayplan.Official, Zone: "global"},
		},
		Standby: []relayplan.Candidate{
			{URL: "cn-canary", Source: relayplan.Official, Zone: "cn", Canary: true},
			{URL: "global-canary", Source: relayplan.Official, Zone: "global", Canary: true},
			{URL: "community", Source: relayplan.Community},
		},
	}
	pending := map[string]*waitingRegistration{
		"cn1":           {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global1":       {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"cn-canary":     {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
		"global-canary": {endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})},
	}
	global1Canceled := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "global1":
			context.AfterFunc(ctx, func() { close(global1Canceled) })
			return pending[url], nil
		case "cn1", "cn-canary", "global-canary":
			return pending[url], nil
		case "community":
			return newEndpoint(), nil
		default:
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, pending["cn1"].observed)
	waitValue(t, pending["global1"].observed)
	pending["cn1"].publish(RegistrationRetrying)
	pending["global1"].publish(RegistrationRetrying)
	// Active wave now fills all 4 network slots: [cn1, global1, cn-canary, global-canary].
	waitValue(t, pending["cn-canary"].observed)
	waitValue(t, pending["global-canary"].observed)
	pending["cn-canary"].publish(RegistrationWaitingAuthorization)
	pending["global-canary"].publish(RegistrationWaitingAuthorization)
	waitValue(t, global1Canceled)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidates := set.Registrations().Candidates
	if set.ReadyRelayURL() != "community" || len(candidates) != relayplan.MaximumNetworkCandidates ||
		slices.Contains(candidates, "global1") || !slices.Contains(candidates, "community") {
		t.Fatal(set.ReadyRelayURL(), candidates)
	}
}
