package relayset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
)

func TestCommunityAdmissionReservesBothOfficialSlots(t *testing.T) {
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "cn", Source: relayplan.Official, Zone: "cn"}, {URL: "global", Source: relayplan.Official, Zone: "global"}, {URL: "community", Source: relayplan.Community, Verified: true}}}
	a := NewNetworkAdmissions(plan, nil, nil)
	var events []Admission
	a.Observe(func(event Admission) { events = append(events, event) })
	for _, url := range []string{"community", "global", "cn"} {
		permit, err := a.Control(url).Begin(t.Context())
		if err != nil || !permit.Admit() {
			t.Fatal(url, err)
		}
		permit.Release()
	}
	a.Control("community").Unavailable()
	want := []Admission{
		{Endpoint: "community", Primary: "", State: AdmissionAdmitted, Reason: AdmissionReasonCommunity, Count: 1},
		{Endpoint: "global", Primary: "global", State: AdmissionAdmitted, Reason: AdmissionReasonPrimary, Count: 2},
		{Endpoint: "cn", Primary: "global", State: AdmissionAdmitted, Reason: AdmissionReasonBackup, Count: 3},
		{Endpoint: "community", Primary: "global", State: AdmissionUnavailable, Reason: AdmissionReasonCommunityLost, Count: 2},
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

func TestCommunityPermitReleaseDoesNotThawCooledOfficialBackup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release func(AttemptPermit)
	}{
		{"failed", func(p AttemptPermit) { p.ReleaseFailed() }},
		{"canceled", func(p AttemptPermit) { p.Release() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				plan := relayplan.Plan{
					Network: []relayplan.Candidate{
						{URL: "cn", Source: relayplan.Official, Zone: "cn"},
						{URL: "global", Source: relayplan.Official, Zone: "global", CooldownUntil: start.Add(time.Minute)},
						{URL: "community", Source: relayplan.Community, Verified: true},
					},
				}
				a := NewNetworkAdmissions(plan, nil, nil)
				primary, err := a.Control("cn").Begin(t.Context())
				if err != nil || !primary.Admit() {
					t.Fatalf("primary admission failed: %v", err)
				}
				primary.Release()

				backup := make(chan AttemptPermit, 1)
				go func() {
					p, _ := a.Control("global").Begin(t.Context())
					backup <- p
				}()
				synctest.Wait()

				community, err := a.Control("community").Begin(t.Context())
				if err != nil {
					t.Fatalf("community begin failed: %v", err)
				}
				tc.release(community)
				synctest.Wait()

				time.Sleep(time.Minute - time.Nanosecond)
				synctest.Wait()
				select {
				case <-backup:
					t.Fatal("community release thawed soft-cooled official backup early")
				default:
				}
				time.Sleep(time.Nanosecond)
				p := <-backup
				if p == nil || !p.Admit() || time.Since(start) != time.Minute {
					t.Fatalf("cooled official backup woke at %v, want %v", time.Since(start), time.Minute)
				}
				p.Release()
			})
		})
	}
}

func TestNetworkAdmissionsBoundsCommunityByRemainingEndpointBudget(t *testing.T) {
	configured := func(n int) []relayplan.Candidate {
		out := make([]relayplan.Candidate, n)
		for i := range n {
			out[i] = relayplan.Candidate{URL: fmt.Sprintf("own-%d", i), Source: relayplan.Configured}
		}
		return out
	}

	t.Run("no remaining slots", func(t *testing.T) {
		for _, count := range []int{MaximumEndpoints, MaximumEndpoints + 1} {
			synctest.Test(t, func(t *testing.T) {
				plan := relayplan.Plan{
					Configured: configured(count),
					Network: []relayplan.Candidate{
						{URL: "cn", Source: relayplan.Official, Zone: "cn"},
						{URL: "community", Source: relayplan.Community, Verified: true},
					},
				}
				a := NewNetworkAdmissions(plan, nil, nil)
				for _, url := range []string{"cn", "community"} {
					ctx, cancel := context.WithCancel(t.Context())
					done := make(chan error, 1)
					go func() {
						_, err := a.Control(url).Begin(ctx)
						done <- err
					}()
					synctest.Wait()
					select {
					case err := <-done:
						t.Fatalf("configured=%d url=%s admitted/exited early: %v", count, url, err)
					default:
					}
					cancel()
					if err := <-done; !errors.Is(err, context.Canceled) {
						t.Fatalf("configured=%d url=%s err=%v, want context.Canceled", count, url, err)
					}
				}
			})
		}
	})

	t.Run("single remaining slot shared across official and community", func(t *testing.T) {
		for _, tc := range []struct {
			name, first, second string
			want                []Admission
		}{
			{
				name:   "community then official",
				first:  "community",
				second: "cn",
				want: []Admission{
					{Endpoint: "community", Primary: "", State: AdmissionAdmitted, Reason: AdmissionReasonCommunity, Count: 1},
					{Endpoint: "cn", Primary: "", State: AdmissionParked, Reason: AdmissionReasonTargetReached, Count: 1},
					{Endpoint: "community", Primary: "", State: AdmissionUnavailable, Reason: AdmissionReasonCommunityLost, Count: 0},
					{Endpoint: "cn", Primary: "cn", State: AdmissionAdmitted, Reason: AdmissionReasonPrimary, Count: 1},
				},
			},
			{
				name:   "official then community",
				first:  "cn",
				second: "community",
				want: []Admission{
					{Endpoint: "cn", Primary: "cn", State: AdmissionAdmitted, Reason: AdmissionReasonPrimary, Count: 1},
					{Endpoint: "community", Primary: "cn", State: AdmissionParked, Reason: AdmissionReasonTargetReached, Count: 1},
					{Endpoint: "cn", Primary: "", State: AdmissionUnavailable, Reason: AdmissionReasonPrimaryLost, Count: 0},
					{Endpoint: "community", Primary: "", State: AdmissionAdmitted, Reason: AdmissionReasonCommunity, Count: 1},
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					plan := relayplan.Plan{
						Configured: configured(MaximumEndpoints - 1),
						Network: []relayplan.Candidate{
							{URL: "cn", Source: relayplan.Official, Zone: "cn"},
							{URL: "community", Source: relayplan.Community, Verified: true},
						},
					}
					a := NewNetworkAdmissions(plan, nil, nil)
					var events []Admission
					a.Observe(func(event Admission) { events = append(events, event) })

					first, err := a.Control(tc.first).Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					second, err := a.Control(tc.second).Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if !first.Admit() {
						t.Fatalf("%s failed to claim single remaining slot", tc.first)
					}
					first.Release()
					if second.Admit() {
						t.Fatalf("%s exceeded remaining endpoint budget after %s admission", tc.second, tc.first)
					}
					second.Release()

					waiting := make(chan AttemptPermit, 1)
					go func() {
						p, _ := a.Control(tc.second).Begin(t.Context())
						waiting <- p
					}()
					synctest.Wait()
					select {
					case <-waiting:
						t.Fatalf("%s started while remaining slot was occupied by %s", tc.second, tc.first)
					default:
					}

					a.Control(tc.first).Unavailable()
					p := <-waiting
					if p == nil || !p.Admit() {
						t.Fatalf("%s failed to claim slot after %s became unavailable", tc.second, tc.first)
					}
					p.Release()

					if !slices.Equal(events, tc.want) {
						t.Fatalf("events = %+v, want %+v", events, tc.want)
					}
				})
			})
		}
	})

	t.Run("two remaining slots cap combined admissions", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			plan := relayplan.Plan{
				Configured: configured(MaximumEndpoints - 2),
				Network: []relayplan.Candidate{
					{URL: "cn", Source: relayplan.Official, Zone: "cn"},
					{URL: "global", Source: relayplan.Official, Zone: "global"},
					{URL: "community", Source: relayplan.Community, Verified: true},
				},
			}
			a := NewNetworkAdmissions(plan, nil, nil)
			comm, err := a.Control("community").Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			cn, err := a.Control("cn").Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			global, err := a.Control("global").Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !comm.Admit() || !cn.Admit() {
				t.Fatal("first two admissions should fit within remaining budget")
			}
			comm.Release()
			cn.Release()
			if global.Admit() {
				t.Fatal("third network admission exceeded MaximumEndpoints")
			}
			global.Release()

			a.Control("community").Unavailable()
			recovered, err := a.Control("global").Begin(t.Context())
			if err != nil || !recovered.Admit() {
				t.Fatalf("backup failed to claim freed slot: %v", err)
			}
			recovered.Release()
			if a.active != 2 || a.communityActive != 0 {
				t.Fatalf("active=%d communityActive=%d, want 2, 0", a.active, a.communityActive)
			}
		})
	})

	t.Run("three remaining slots keep official pair and community independent", func(t *testing.T) {
		plan := relayplan.Plan{
			Configured: configured(MaximumEndpoints - 3),
			Network: []relayplan.Candidate{
				{URL: "cn", Source: relayplan.Official, Zone: "cn"},
				{URL: "global", Source: relayplan.Official, Zone: "global"},
				{URL: "community", Source: relayplan.Community, Verified: true},
			},
		}
		a := NewNetworkAdmissions(plan, nil, nil)
		for _, url := range []string{"community", "global", "cn"} {
			permit, err := a.Control(url).Begin(t.Context())
			if err != nil || !permit.Admit() {
				t.Fatalf("%s failed to admit with 3 remaining slots: %v", url, err)
			}
			permit.Release()
		}
		if a.active != 2 || a.communityActive != 1 {
			t.Fatalf("active=%d communityActive=%d, want 2, 1", a.active, a.communityActive)
		}
	})
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
	fallbacks := make(chan Fallback, 4)
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
	}, func(event Fallback) { fallbacks <- event })
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
	wantFallbacks := []struct {
		next string
		tier relayplan.Tier
	}{
		{"comm1", relayplan.TierCommunityNamed},
		{"comm2", relayplan.TierCommunityNamed},
		{"comm-ip", relayplan.TierCommunityIPLiteral},
	}
	for _, want := range wantFallbacks {
		event := waitValue(t, fallbacks)
		if event.Reason != FallbackReasonOfficialAuthWait || event.Tier != want.tier || !slices.Equal(event.Next, []string{want.next}) {
			t.Fatalf("unexpected auth-wait fallback event: %+v (want next=%s tier=%s)", event, want.next, want.tier)
		}
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

type trackingRetiredEndpoint struct {
	*endpoint
	observeCalls int
	waitCalls    int
}

func (e *trackingRetiredEndpoint) ObserveRegistration(observer func(RegistrationState)) {
	e.mu.Lock()
	e.observeCalls++
	e.mu.Unlock()
	e.endpoint.ObserveRegistration(observer)
}

func (e *trackingRetiredEndpoint) WaitReady(ctx context.Context) error {
	e.mu.Lock()
	e.waitCalls++
	e.mu.Unlock()
	return e.endpoint.WaitReady(ctx)
}

func TestFreezeRegistrationsRetiresExcludedPendingRoutesAndUpdatesStatus(t *testing.T) {
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: "official", Source: relayplan.Official, Zone: "cn"},
			{URL: "comm-verified", Source: relayplan.Community, Verified: true},
			{URL: "comm-unverified", Source: relayplan.Community},
			{URL: "comm-failed", Source: relayplan.Community},
		},
	}
	verified := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	unverifiedDialed := make(chan struct{})
	unverifiedCanceled := make(chan struct{})
	releaseUnverified := make(chan struct{})
	racedEndpoint := &trackingRetiredEndpoint{endpoint: newEndpoint()}
	failedDone := make(chan struct{})

	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "official":
			return newEndpoint(), nil
		case "comm-verified":
			return verified, nil
		case "comm-unverified":
			close(unverifiedDialed)
			<-ctx.Done()
			close(unverifiedCanceled)
			<-releaseUnverified
			// Simulate dial returning a freshly constructed endpoint right after retirement.
			return racedEndpoint, nil
		case "comm-failed":
			close(failedDone)
			return nil, errors.New("probe failed")
		default:
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	statusChanges := make(chan SenderStatus, 16)
	set.ObserveStatus(func(status SenderStatus) {
		statusChanges <- status
	})
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitValue(t, verified.observed)
	waitValue(t, unverifiedDialed)
	waitValue(t, failedDone)
	for set.Status().Availability.Terminal != 1 {
		waitValue(t, statusChanges)
	}
	// Drain earlier notifications before asserting the freeze transition.
	for len(statusChanges) > 0 {
		<-statusChanges
	}

	// Even if keep returns false for firstReady, the publishing route stays protected.
	frozen := set.FreezeRegistrations(func(url string) bool {
		return url != "comm-unverified" && url != "official"
	})
	waitValue(t, unverifiedCanceled)

	if !slices.Equal(frozen.Candidates, []string{"comm-verified", "official"}) ||
		!slices.Equal(frozen.Admitted, []string{"official"}) ||
		!slices.Equal(frozen.Rejected, []string{"comm-failed"}) {
		t.Fatalf("frozen registrations = %+v", frozen)
	}
	status := waitValue(t, statusChanges)
	if status.Availability.Total != 3 || status.Availability.Available != 1 || status.Availability.Terminal != 1 {
		t.Fatalf("status after freeze = %+v, want Total=3 Available=1 Terminal=1", status)
	}

	close(releaseUnverified)
	// Verified pending route remains active and can still admit after publication.
	verified.publish(RegistrationAvailable)
	close(verified.ready)
	if got := waitValue(t, statusChanges); got.Availability.Available != 2 || got.Availability.Total != 3 {
		t.Fatalf("status after verified admission = %+v", got)
	}

	cleanupSet(t, set)
	racedEndpoint.mu.Lock()
	observeCalls, waitCalls, stopped, cleaned := racedEndpoint.observeCalls, racedEndpoint.waitCalls, racedEndpoint.stopped, racedEndpoint.cleaned
	racedEndpoint.mu.Unlock()
	if observeCalls != 0 || waitCalls != 0 || !stopped || !cleaned {
		t.Fatalf("retired raced endpoint was not stopped cleanly before WaitReady: observe=%d wait=%d stopped=%v cleaned=%v",
			observeCalls, waitCalls, stopped, cleaned)
	}
}
