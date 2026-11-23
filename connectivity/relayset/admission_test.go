package relayset

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/transport/relayv2"
)

func admissionFixture() relayplan.Plan {
	return relayplan.Plan{Network: []relayplan.Candidate{
		{URL: "https://a.example", Zone: "global", Provider: "a", Operator: "one"},
		{URL: "https://b.example", Zone: "global", Provider: "b", Operator: "two"},
		{URL: "https://c.example", Zone: "cn", Provider: "c", Operator: "one"},
		{URL: "https://d.example", Zone: "global", Provider: "a", Operator: "one"},
	}}
}

func beginCandidates(t *testing.T, a *NetworkAdmissions, plan relayplan.Plan) []AttemptPermit {
	t.Helper()
	var result []AttemptPermit
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
	a := NewNetworkAdmissions(plan, nil, nil)
	var events []Admission
	a.Observe(func(event Admission) { events = append(events, event) })
	permits := beginCandidates(t, a, plan)
	if !permits[2].Admit() {
		t.Fatal("first successful candidate was not primary")
	}
	if permits[0].Admit() {
		t.Fatal("same-operator candidate displaced a more diverse backup")
	}
	permits[0].Release()
	if permits[0].Admit() {
		t.Fatal("released permit was admitted")
	}
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
	a.Control(plan.Network[2].URL).Unavailable()
	want := []struct{ endpoint, state, reason string }{
		{plan.Network[2].URL, AdmissionAdmitted, AdmissionReasonPrimary},
		{plan.Network[0].URL, AdmissionParked, AdmissionReasonBackupNotChosen},
		{plan.Network[0].URL, AdmissionParked, AdmissionReasonPermitReleased},
		{plan.Network[1].URL, AdmissionAdmitted, AdmissionReasonBackup},
		{plan.Network[3].URL, AdmissionParked, AdmissionReasonTargetReached},
		{plan.Network[2].URL, AdmissionUnavailable, AdmissionReasonPrimaryLost},
		{plan.Network[2].URL, AdmissionAdmitted, AdmissionReasonBackup},
		{plan.Network[2].URL, AdmissionUnavailable, AdmissionReasonBackupLost},
	}
	if len(events) != len(want) {
		t.Fatalf("admission events = %+v, want %d", events, len(want))
	}
	for i, w := range want {
		if events[i].Endpoint != w.endpoint || events[i].State != w.state || events[i].Reason != w.reason {
			t.Fatalf("event[%d] = %+v, want %+v", i, events[i], w)
		}
	}
}

func TestFailedPreferredBackupReleasesAttemptForFallback(t *testing.T) {
	plan := admissionFixture()
	a := NewNetworkAdmissions(plan, nil, nil)
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

func TestDeferredRegistrationHonorsSoftAndServerDeadlines(t *testing.T) {
	const (
		active = iota
		failed
		disconnected
	)
	for _, tc := range []struct {
		name         string
		fallback     bool
		primary      int
		server, want time.Duration
	}{
		{"ready primary", false, active, 0, time.Minute},
		{"published primary disconnected", false, disconnected, 0, 0},
		{"published primary lost with server deadline", false, disconnected, 12 * time.Second, 12 * time.Second},
		{"preferred failed or waiting for authorization", false, failed, 0, 0},
		{"released primary with server deadline", false, failed, 12 * time.Second, 12 * time.Second},
		{"all locally cooled", true, active, 0, 0},
		{"all cooled with explicit deadline", true, active, 15 * time.Second, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				plan := admissionFixture()
				plan.Network = plan.Network[:2]
				start := time.Now()
				plan.Decision.CooldownFallback = tc.fallback
				plan.Network[1].CooldownUntil = start.Add(time.Minute)
				if tc.server > 0 {
					plan.Network[1].ServerRetryAt = start.Add(tc.server)
				}
				a := NewNetworkAdmissions(plan, nil, nil)
				primary, err := a.Control(plan.Network[0].URL).Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				expectWaiting := func(inflight int) {
					t.Helper()
					a.mu.Lock()
					defer a.mu.Unlock()
					if a.inflight != inflight || a.states[1].eligible {
						t.Fatal("waiting backup entered the race", a.inflight, a.states[1])
					}
				}
				backup := make(chan AttemptPermit, 1)
				go func() {
					p, _ := a.Control(plan.Network[1].URL).Begin(t.Context())
					backup <- p
				}()
				synctest.Wait()
				if !tc.fallback || tc.server > 0 {
					expectWaiting(1)
				}
				if tc.primary != failed && !primary.Admit() {
					t.Fatal("primary admission failed")
				}
				primary.Release()
				if tc.primary == disconnected {
					a.Control(plan.Network[0].URL).Unavailable()
				}
				synctest.Wait()
				if tc.want > 0 {
					time.Sleep(tc.want - time.Nanosecond)
					synctest.Wait()
					expectWaiting(0)
					time.Sleep(time.Nanosecond)
				}
				p := <-backup
				if p == nil || !p.Admit() || time.Since(start) != tc.want {
					t.Fatal("backup failed to thaw", time.Since(start))
				}
				p.Release()
			})
		})
	}
}

func TestStandbyDialRechecksConcurrentHTTPDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := admissionFixture()
		start := time.Now()
		first := start.Add(time.Second)
		plan.Network[1].ServerRetryAt = first
		plan.Network[1].Identity = "delayed"
		plan.Network[1].Canary = true
		plan.Standby = plan.Network[1:2]
		plan.Network = plan.Network[:1]
		var latest atomic.Int64
		lookup := func(identity string) time.Time {
			if identity == "delayed" {
				return time.Unix(0, latest.Load())
			}
			return time.Time{}
		}
		a := NewNetworkAdmissions(plan, nil, lookup)
		done := make(chan AttemptPermit, 1)
		go func() { p, _ := a.Control(plan.Standby[0].URL).Begin(t.Context()); done <- p }()
		synctest.Wait()
		latest.Store(first.Add(time.Second).UnixNano())
		p, err := a.Control(plan.Network[0].URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		p.Release()
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("dialed at superseded server deadline")
		default:
		}
		p = <-done
		if time.Since(start) != 2*time.Second {
			t.Fatal("delayed dial", time.Since(start))
		}
		p.Release()
	})
}

func TestReleaseFailedDeprioritizesReenteringCandidateUntilHandshakeSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := relayplan.Plan{
			Network: []relayplan.Candidate{
				{URL: "https://active0.example", Zone: "global", Provider: "a", Operator: "one"},
				{URL: "https://active1.example", Zone: "cn", Provider: "b", Operator: "two"},
			},
			Standby: []relayplan.Candidate{
				{URL: "https://canary0.example", Zone: "global", Provider: "c", Operator: "three", Canary: true},
				{URL: "https://canary1.example", Zone: "cn", Provider: "d", Operator: "four", Canary: true},
			},
		}
		a := NewNetworkAdmissions(plan, nil, nil)
		active0, err := a.Control(plan.Network[0].URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		active1, err := a.Control(plan.Network[1].URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		active0.ReleaseFailed()
		active1.ReleaseFailed()
		// A stale released permit must not clear failure history.
		if active1.Admit() {
			t.Fatal("released permit admitted")
		}
		canary0, err := a.Control(plan.Standby[0].URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		canary1, err := a.Control(plan.Standby[1].URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !canary0.Admit() {
			t.Fatal("primary canary refused")
		}
		canary0.Release()

		retryActive1 := make(chan AttemptPermit, 1)
		go func() {
			p, _ := a.Control(plan.Network[1].URL).Begin(t.Context())
			retryActive1 <- p
		}()
		synctest.Wait()
		select {
		case <-retryActive1:
			t.Fatal("failed active preempted healthy standby canary")
		default:
		}
		if !canary1.Admit() {
			t.Fatal("healthy standby canary parked behind failed active")
		}
		canary1.Release()

		// Once the backup canary drops and fails to reconnect, the waiting active
		// can retry, and completing a handshake clears its earlier failure.
		a.Control(plan.Standby[1].URL).Unavailable()
		canaryRetry, err := a.Control(plan.Standby[1].URL).Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		canaryRetry.ReleaseFailed()
		synctest.Wait()
		recovered := <-retryActive1
		if recovered == nil || !recovered.Admit() {
			t.Fatal("active failed to recover lost backup slot after canary failed")
		}
		recovered.Release()
		if a.candidateForBackup(1).HistoryState() == "failed" {
			t.Fatalf("admitted candidate retained failed history: %+v", a.states[1])
		}
	})
}

func startPlannedAdmissionSender(
	t *testing.T,
	ctx context.Context,
	plan relayplan.Plan,
	admissions *NetworkAdmissions,
	dial func(context.Context, string) (SenderConnection, error),
	auth map[string]AuthorizationSource,
) *Sender {
	t.Helper()
	set, err := NewPlannedSender(ctx, plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		cfg := newSenderRelayTestConfig(t, nil, &senderRelayTestDialer{
			dial: func(dialCtx context.Context, req relayv2.SenderConfig) (SenderConnection, error) {
				return dial(dialCtx, req.RelayBaseURL)
			},
		}, nil)
		cfg.Candidate = Candidate{PublicURL: url, Authorization: auth[url]}
		cfg.Attempts = admissions.Control(url)
		cfg.Jitter = func(delay time.Duration) time.Duration { return delay }
		return NewSenderLifecycle(cfg)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		set.StopRecovery()
		set.workers.Wait()
	})
	return set
}

func waitDialDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func blockingTestConnection() SenderConnection {
	return NewSenderConnection(&senderRelayTestEndpoint{
		accept: func(ctx context.Context) (*relayv2.Channel, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
}

func TestPlannedSenderCanaryBackupIsNotStarvedByRetryingFailedActives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := relayplan.Plan{
			Network: []relayplan.Candidate{
				{URL: "https://active0.example", Zone: "global", Provider: "a", Operator: "one"},
				{URL: "https://active1.example", Zone: "cn", Provider: "b", Operator: "two"},
			},
			Standby: []relayplan.Candidate{
				{URL: "https://canary0.example", Zone: "global", Provider: "c", Operator: "three", Canary: true},
				{URL: "https://canary1.example", Zone: "cn", Provider: "d", Operator: "four", Canary: true},
			},
		}
		admissions := NewNetworkAdmissions(plan, nil, nil)
		var mu sync.Mutex
		var events []Admission
		admissions.Observe(func(event Admission) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		})
		dialCounts := map[string]*atomic.Int32{
			plan.Network[0].URL: {},
			plan.Network[1].URL: {},
			plan.Standby[0].URL: {},
			plan.Standby[1].URL: {},
		}
		start := time.Now()
		set := startPlannedAdmissionSender(t, t.Context(), plan, admissions, func(ctx context.Context, url string) (SenderConnection, error) {
			attempt := dialCounts[url].Add(1)
			switch url {
			case plan.Network[0].URL:
				if attempt == 1 {
					return SenderConnection{}, errors.New("connection refused")
				}
				// Subsequent retries simulate a blackholed active timing out at 10s.
				<-ctx.Done()
				return SenderConnection{}, ctx.Err()
			case plan.Network[1].URL:
				if attempt == 1 {
					// Fail after active0 has already woken from its 100ms retry backoff,
					// so active0 is already eligible mid-dial when canaries start.
					if err := waitDialDelay(ctx, 150*time.Millisecond); err != nil {
						return SenderConnection{}, err
					}
					return SenderConnection{}, errors.New("tls handshake reset")
				}
				<-ctx.Done()
				return SenderConnection{}, ctx.Err()
			case plan.Standby[0].URL:
				if err := waitDialDelay(ctx, 50*time.Millisecond); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			case plan.Standby[1].URL:
				// Complete after active1 also wakes from its 100ms fast retry clock.
				if err := waitDialDelay(ctx, 250*time.Millisecond); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			default:
				return SenderConnection{}, errors.New("unexpected relay")
			}
		}, nil)

		if err := set.WaitReady(t.Context()); err != nil {
			t.Fatal(err)
		}
		if set.ReadyRelayURL() != plan.Standby[0].URL {
			t.Fatalf("primary = %s, want %s", set.ReadyRelayURL(), plan.Standby[0].URL)
		}
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()

		status := set.Status()
		got := set.Registrations().Admitted
		want := []string{plan.Standby[0].URL, plan.Standby[1].URL}
		if status.Availability.Available != 2 || !slices.Equal(got, want) {
			t.Fatalf("after %v: status=%+v admitted=%v want=%v", time.Since(start), status, got, want)
		}
		mu.Lock()
		for _, event := range events {
			if event.Endpoint == plan.Standby[1].URL && event.Reason == AdmissionReasonBackupNotChosen {
				t.Fatalf("healthy standby canary was parked by failed active: %+v", events)
			}
		}
		mu.Unlock()

		// Once both standby canaries are admitted, failed actives must not keep retrying.
		before0, before1 := dialCounts[plan.Network[0].URL].Load(), dialCounts[plan.Network[1].URL].Load()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if after0, after1 := dialCounts[plan.Network[0].URL].Load(), dialCounts[plan.Network[1].URL].Load(); after0 != before0 || after1 != before1 {
			t.Fatalf("failed actives kept dialing after target reached: active0 %d->%d, active1 %d->%d", before0, after0, before1, after1)
		}
	})
}

func TestPlannedSenderActiveFallbackIsNotStarvedByFailedPreferredZone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := relayplan.Plan{
			Network: []relayplan.Candidate{
				{URL: "https://primary.example", Zone: "global", Provider: "a", Operator: "one"},
				{URL: "https://preferred.example", Zone: "cn", Provider: "c", Operator: "one"},
				{URL: "https://fallback.example", Zone: "global", Provider: "b", Operator: "two"},
			},
		}
		admissions := NewNetworkAdmissions(plan, nil, nil)
		preferredDials := atomic.Int32{}
		fallbackDials := atomic.Int32{}
		set := startPlannedAdmissionSender(t, t.Context(), plan, admissions, func(ctx context.Context, url string) (SenderConnection, error) {
			switch url {
			case plan.Network[0].URL:
				if err := waitDialDelay(ctx, 20*time.Millisecond); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			case plan.Network[1].URL:
				if preferredDials.Add(1) == 1 {
					if err := waitDialDelay(ctx, 50*time.Millisecond); err != nil {
						return SenderConnection{}, err
					}
					return SenderConnection{}, errors.New("preferred zone unreachable")
				}
				<-ctx.Done()
				return SenderConnection{}, ctx.Err()
			case plan.Network[2].URL:
				// First dial finishes at 40ms while preferred is still healthy (parked),
				// second dial starts at 50ms and finishes at 250ms after preferred wakes at 150ms.
				delay := 40 * time.Millisecond
				if fallbackDials.Add(1) > 1 {
					delay = 200 * time.Millisecond
				}
				if err := waitDialDelay(ctx, delay); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			default:
				return SenderConnection{}, errors.New("unexpected relay")
			}
		}, nil)

		if err := set.WaitReady(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()

		want := []string{plan.Network[2].URL, plan.Network[0].URL}
		if got := set.Registrations().Admitted; !slices.Equal(got, want) {
			t.Fatalf("admitted = %v, want %v", got, want)
		}
		if got := fallbackDials.Load(); got != 2 {
			t.Fatalf("fallback relay dialed %d times, want 2 (initial park + admitted retry)", got)
		}
		if got := preferredDials.Load(); got != 1 {
			t.Fatalf("failed preferred relay dialed %d times, want 1", got)
		}
	})
}

func TestAuthorizationWaitDoesNotPenalizeCandidateInBackupRanking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := relayplan.Plan{
			Network: []relayplan.Candidate{
				{URL: "https://primary.example", Zone: "global", Provider: "a", Operator: "one"},
				{URL: "https://preferred.example", Zone: "cn", Provider: "c", Operator: "one"},
				{URL: "https://fallback.example", Zone: "global", Provider: "b", Operator: "two"},
			},
		}
		admissions := NewNetworkAdmissions(plan, nil, nil)
		preferredAuth := &changingAuthorization{
			value:   Authorization{State: AuthorizationPendingAttribution},
			changed: make(chan struct{}),
		}
		set := startPlannedAdmissionSender(t, t.Context(), plan, admissions, func(ctx context.Context, url string) (SenderConnection, error) {
			switch url {
			case plan.Network[0].URL:
				if err := waitDialDelay(ctx, 20*time.Millisecond); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			case plan.Network[1].URL:
				if err := waitDialDelay(ctx, 40*time.Millisecond); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			case plan.Network[2].URL:
				if err := waitDialDelay(ctx, 200*time.Millisecond); err != nil {
					return SenderConnection{}, err
				}
				return blockingTestConnection(), nil
			default:
				return SenderConnection{}, errors.New("unexpected relay")
			}
		}, map[string]AuthorizationSource{plan.Network[1].URL: preferredAuth})

		if err := set.WaitReady(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Unblock authorization while fallback is still dialing: preferred must keep
		// its unfailed priority and claim the diverse backup slot.
		time.Sleep(30 * time.Millisecond)
		synctest.Wait()
		preferredAuth.set(Authorization{State: AuthorizationReady, Bearer: "ticket"})
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()

		want := []string{plan.Network[1].URL, plan.Network[0].URL}
		if got := set.Registrations().Admitted; !slices.Equal(got, want) {
			t.Fatalf("admitted = %v, want %v", got, want)
		}
	})
}

func TestCandidateFailingOrStoppingBeforeBeginDoesNotBlockBackup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := relayplan.Plan{
			Network: []relayplan.Candidate{
				{URL: "https://primary.example", Zone: "global", Provider: "a", Operator: "one"},
				{URL: "https://dial-failed.example", Zone: "cn", Provider: "c", Operator: "two"},
				{URL: "https://stopped-early.example", Zone: "cn", Provider: "d", Operator: "three"},
				{URL: "https://fallback.example", Zone: "global", Provider: "b", Operator: "two"},
			},
		}
		admissions := NewNetworkAdmissions(plan, nil, nil)
		var mu sync.Mutex
		var events []Admission
		admissions.Observe(func(event Admission) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		})
		set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
			if url == plan.Network[1].URL {
				return nil, errors.New("endpoint setup failed before Begin")
			}
			cfg := newSenderRelayTestConfig(t, nil, &senderRelayTestDialer{
				dial: func(dialCtx context.Context, req relayv2.SenderConfig) (SenderConnection, error) {
					delay := 20 * time.Millisecond
					if req.RelayBaseURL == plan.Network[3].URL {
						delay = 50 * time.Millisecond
					}
					if err := waitDialDelay(dialCtx, delay); err != nil {
						return SenderConnection{}, err
					}
					return blockingTestConnection(), nil
				},
			}, nil)
			cfg.Candidate = Candidate{PublicURL: url}
			cfg.Attempts = admissions.Control(url)
			lifecycle, err := NewSenderLifecycle(cfg)
			if err != nil {
				return nil, err
			}
			if url == plan.Network[2].URL {
				lifecycle.StopRecovery()
			}
			return lifecycle, nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			set.StopRecovery()
			set.workers.Wait()
		}()

		if err := set.WaitReady(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		want := []string{plan.Network[3].URL, plan.Network[0].URL}
		if got := set.Registrations().Admitted; !slices.Equal(got, want) {
			t.Fatalf("admitted = %v, want %v", got, want)
		}
		if status := set.Status(); status.Availability.Available != 2 {
			t.Fatalf("available = %d, want 2 (%+v)", status.Availability.Available, status)
		}
		mu.Lock()
		for _, event := range events {
			if event.Endpoint == plan.Network[3].URL && event.Reason == AdmissionReasonBackupNotChosen {
				t.Fatalf("healthy fallback was blocked by candidate that never entered Begin: %+v", events)
			}
		}
		mu.Unlock()
	})

	// Also verify the Begin() gate directly when a candidate enters Begin only
	// after primary is already admitted while a higher-ranked candidate never starts.
	synctest.Test(t, func(t *testing.T) {
		plan := relayplan.Plan{
			Network: []relayplan.Candidate{
				{URL: "https://primary.example", Zone: "global", Provider: "a", Operator: "one"},
				{URL: "https://never-begun.example", Zone: "cn", Provider: "c", Operator: "two"},
				{URL: "https://fallback.example", Zone: "global", Provider: "b", Operator: "two"},
			},
		}
		a := NewNetworkAdmissions(plan, nil, nil)
		primary, err := a.Control(plan.Network[0].URL).Begin(t.Context())
		if err != nil || !primary.Admit() {
			t.Fatalf("primary admission failed: %v", err)
		}
		primary.Release()
		a.Control(plan.Network[1].URL).Unavailable()

		fallback, err := a.Control(plan.Network[2].URL).Begin(t.Context())
		if err != nil || !fallback.Admit() {
			t.Fatalf("fallback blocked in Begin/Admit by unstarted candidate: %v", err)
		}
		fallback.Release()
	})
}
