package relayset

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
				backup := make(chan senderrelay.AttemptPermit, 1)
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

func TestPendingDialRechecksConcurrentHTTPDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := admissionFixture()
		start := time.Now()
		first := start.Add(time.Second)
		plan.Network[1].ServerRetryAt = first
		plan.Network[1].Identity = "delayed"
		var latest atomic.Int64
		lookup := func(identity string) time.Time {
			if identity == "delayed" {
				return time.Unix(0, latest.Load())
			}
			return time.Time{}
		}
		a := NewNetworkAdmissions(plan, nil, lookup)
		done := make(chan senderrelay.AttemptPermit, 1)
		go func() { p, _ := a.Control(plan.Network[1].URL).Begin(t.Context()); done <- p }()
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
