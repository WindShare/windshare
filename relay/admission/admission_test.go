package admission

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func testConfig(clock Clock) Config {
	cfg := DefaultConfig()
	cfg.Clock = clock
	cfg.MaxConnections = 2
	cfg.MaxConcurrentShares = 3
	cfg.MaxSharesPerSource = 2
	cfg.MaxManifestBytesPerSource = 10
	cfg.MaxTotalManifestBytes = 16
	cfg.RegisterPerSource = Rate{PerSecond: 1, Burst: 8}
	cfg.JoinPerSource = Rate{PerSecond: 1, Burst: 2}
	cfg.JoinPerShare = Rate{PerSecond: 1, Burst: 2}
	return cfg
}

func mustController(t *testing.T, cfg Config) *Controller {
	t.Helper()
	c, err := NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func allowJoin(c *Controller, source string, lease *Lease) Decision {
	attempt, decision := c.BeginJoin(source)
	if !decision.Allowed() || lease == nil {
		return decision
	}
	return attempt.AllowShare(lease)
}

func TestConfigValidation(t *testing.T) {
	valid := DefaultConfig()
	for name, mutate := range map[string]func(*Config){
		"connections":     func(c *Config) { c.MaxConnections = 0 },
		"shares":          func(c *Config) { c.MaxConcurrentShares = 0 },
		"source shares":   func(c *Config) { c.MaxSharesPerSource = 0 },
		"source manifest": func(c *Config) { c.MaxManifestBytesPerSource = c.MaxTotalManifestBytes + 1 },
		"register rate":   func(c *Config) { c.RegisterPerSource.PerSecond = 0 },
		"join burst":      func(c *Config) { c.JoinPerShare.Burst = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := NewController(cfg); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestConnectionLeaseReleasesExactlyOnce(t *testing.T) {
	c := mustController(t, testConfig(&fakeClock{now: time.Unix(1, 0)}))
	first, decision := c.AdmitConnection("ip-a")
	if !decision.Allowed() {
		t.Fatalf("first connection denied: %v", decision)
	}
	second, decision := c.AdmitConnection("ip-b")
	if !decision.Allowed() {
		t.Fatalf("second connection denied: %v", decision)
	}
	if _, decision = c.AdmitConnection("ip-c"); decision != ConnectionCapacityExceeded {
		t.Fatalf("decision = %v", decision)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(first.Release)
	}
	wg.Wait()
	if got := c.Snapshot().Connections; got != 1 {
		t.Fatalf("connections = %d", got)
	}
	third, decision := c.AdmitConnection("ip-c")
	if !decision.Allowed() {
		t.Fatalf("released slot was not reusable: %v", decision)
	}
	second.Release()
	third.Release()
}

func TestShareLeasePinDefersAccountingReleaseExactlyOnce(t *testing.T) {
	c := mustController(t, testConfig(&fakeClock{now: time.Unix(1, 0)}))
	lease, decision := c.AdmitRegister("source", 3, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	first, ok := lease.Pin()
	if !ok {
		t.Fatal("live share lease could not be pinned")
	}
	second, ok := lease.Pin()
	if !ok {
		t.Fatal("live share lease could not be pinned twice")
	}
	lease.Release()
	lease.Release()
	if snapshot := c.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != 3 {
		t.Fatalf("owner release reclaimed pinned accounting: %+v", snapshot)
	}
	if _, ok := lease.Pin(); ok {
		t.Fatal("released owner accepted a new pin")
	}
	first.Release()
	first.Release()
	if snapshot := c.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != 3 {
		t.Fatalf("first pin reclaimed accounting early: %+v", snapshot)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(second.Release)
	}
	wg.Wait()
	if snapshot := c.Snapshot(); snapshot.Shares != 0 || snapshot.ManifestBytes != 0 || snapshot.Sources != 0 {
		t.Fatalf("last pin did not reclaim accounting: %+v", snapshot)
	}
}

func TestShareLeaseEnforcesGlobalAndSourceBudgets(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	c := mustController(t, testConfig(clock))
	a1, decision := c.AdmitRegister("ip-a", 6, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	a2, decision := c.AdmitRegister("ip-a", 4, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	if _, decision = c.AdmitRegister("ip-a", 0, nil); decision != SourceShareCapacityExceeded {
		t.Fatalf("source share decision = %v", decision)
	}
	b1, decision := c.AdmitRegister("ip-b", 6, nil)
	if !decision.Allowed() {
		t.Fatalf("unrelated source was denied: %v", decision)
	}
	if _, decision = c.AdmitRegister("ip-c", 1, nil); decision != ConcurrentShareCapacityExceeded {
		t.Fatalf("global share decision = %v", decision)
	}

	// Reconnect keeps the original reservation even while every global slot is full.
	if got, decision := c.AdmitRegister("ip-new", 6, a1); !decision.Allowed() || got != a1 {
		t.Fatalf("retained lease = %p/%v, want %p/allowed", got, decision, a1)
	}
	a1.Release()
	a1.Release()
	if snapshot := c.Snapshot(); snapshot.Shares != 2 || snapshot.ManifestBytes != 10 {
		t.Fatalf("snapshot after release = %+v", snapshot)
	}
	if _, decision = c.AdmitRegister("ip-c", 7, nil); decision != ManifestCapacityExceeded {
		t.Fatalf("global manifest decision = %v", decision)
	}
	if c.Snapshot().Sources != 2 {
		t.Fatalf("denied register leaked source usage: %+v", c.Snapshot())
	}
	a2.Release()
	b1.Release()
}

func TestSourceManifestBudgetDoesNotDenyOtherSources(t *testing.T) {
	cfg := testConfig(&fakeClock{now: time.Unix(1, 0)})
	cfg.MaxSharesPerSource = 10
	cfg.MaxConcurrentShares = 10
	c := mustController(t, cfg)
	a, decision := c.AdmitRegister("ip-a", 10, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	if _, decision = c.AdmitRegister("ip-a", 1, nil); decision != SourceManifestCapacityExceeded {
		t.Fatalf("source manifest decision = %v", decision)
	}
	b, decision := c.AdmitRegister("ip-b", 1, nil)
	if !decision.Allowed() {
		t.Fatalf("other source denied: %v", decision)
	}
	a.Release()
	b.Release()
}

func TestRegisterRateRefillsWithInjectedClock(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	cfg := testConfig(clock)
	cfg.RegisterPerSource = Rate{PerSecond: 1, Burst: 1}
	c := mustController(t, cfg)
	lease, decision := c.AdmitRegister("ip-a", 1, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	if _, decision = c.AdmitRegister("ip-a", 1, lease); decision != RegisterRateExceeded {
		t.Fatalf("decision = %v", decision)
	}
	clock.now = clock.now.Add(time.Second)
	if got, decision := c.AdmitRegister("ip-a", 1, lease); !decision.Allowed() || got != lease {
		t.Fatalf("refilled decision = %v", decision)
	}
	lease.Release()
}

func TestJoinBucketsAreAtomicAndIsolated(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	cfg := testConfig(clock)
	cfg.JoinPerSource = Rate{PerSecond: 1, Burst: 1}
	cfg.JoinPerShare = Rate{PerSecond: 1, Burst: 2}
	c := mustController(t, cfg)
	shareA, decision := c.AdmitRegister("owner-a", 0, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	shareB, decision := c.AdmitRegister("owner-b", 0, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	defer shareA.Release()
	defer shareB.Release()
	if decision := allowJoin(c, "ip-a", shareA); !decision.Allowed() {
		t.Fatal(decision)
	}
	if decision := allowJoin(c, "ip-a", shareA); decision != JoinRateExceeded {
		t.Fatalf("source decision = %v", decision)
	}
	if decision := allowJoin(c, "ip-a", nil); decision != JoinRateExceeded {
		t.Fatalf("exhausted source decision = %v", decision)
	}
	if decision := allowJoin(c, "ip-b", shareA); !decision.Allowed() {
		t.Fatalf("other source denied: %v", decision)
	}
	if decision := allowJoin(c, "ip-c", shareB); !decision.Allowed() {
		t.Fatalf("other source/share denied: %v", decision)
	}
	clock.now = clock.now.Add(time.Second)
	if decision := allowJoin(c, "ip-a", shareA); !decision.Allowed() {
		t.Fatalf("refill denied: %v", decision)
	}
}

func TestShareRateDenialStillChargesSourceAttempt(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	cfg := testConfig(clock)
	cfg.JoinPerSource = Rate{PerSecond: 1, Burst: 2}
	cfg.JoinPerShare = Rate{PerSecond: 1, Burst: 1}
	c := mustController(t, cfg)
	shareA, decision := c.AdmitRegister("owner-a", 0, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	shareB, decision := c.AdmitRegister("owner-b", 0, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	defer shareA.Release()
	defer shareB.Release()
	if decision := allowJoin(c, "ip-a", shareA); !decision.Allowed() {
		t.Fatal(decision)
	}
	if decision := allowJoin(c, "ip-a", shareA); decision != JoinRateExceeded {
		t.Fatalf("share denial = %v", decision)
	}
	if decision := allowJoin(c, "ip-a", shareB); decision != JoinRateExceeded {
		t.Fatalf("source should be exhausted after two attempts: %v", decision)
	}
}

func TestStagedRegistrationAccountsPendingAndRollsBack(t *testing.T) {
	cfg := testConfig(&fakeClock{now: time.Unix(1, 0)})
	cfg.MaxConcurrentShares = 1
	cfg.MaxSharesPerSource = 1
	cfg.MaxManifestBytesPerSource = 4
	cfg.MaxTotalManifestBytes = 4
	c := mustController(t, cfg)

	pending, decision := c.BeginRegister("ip-a")
	if !decision.Allowed() || pending.ReserveShare() != Allowed || pending.ReserveManifestBytes(3) != Allowed {
		t.Fatalf("failed to stage register: %v, snapshot=%+v", decision, c.Snapshot())
	}
	if snapshot := c.Snapshot(); snapshot.PendingShares != 1 || snapshot.PendingManifestBytes != 3 || snapshot.Shares != 0 {
		t.Fatalf("pending snapshot = %+v", snapshot)
	}

	other, decision := c.BeginRegister("ip-b")
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	if decision := other.ReserveShare(); decision != ConcurrentShareCapacityExceeded {
		t.Fatalf("pending share did not hold concurrency: %v", decision)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(pending.Release)
	}
	wg.Wait()
	if snapshot := c.Snapshot(); snapshot.PendingShares != 0 || snapshot.PendingManifestBytes != 0 || snapshot.Sources != 0 {
		t.Fatalf("rollback leaked accounting: %+v", snapshot)
	}

	if decision := other.ReserveShare(); !decision.Allowed() {
		t.Fatalf("released pending slot was not reusable: %v", decision)
	}
	if decision := other.ReserveManifestBytes(2); !decision.Allowed() {
		t.Fatal(decision)
	}
	lease, decision := other.Commit(nil)
	if !decision.Allowed() || lease == nil {
		t.Fatalf("commit = %p/%v", lease, decision)
	}
	other.Release()
	if snapshot := c.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != 2 || snapshot.PendingShares != 0 || snapshot.PendingManifestBytes != 0 {
		t.Fatalf("committed snapshot = %+v", snapshot)
	}
	lease.Release()
	if snapshot := c.Snapshot(); snapshot.Shares != 0 || snapshot.ManifestBytes != 0 || snapshot.Sources != 0 {
		t.Fatalf("lease release leaked accounting: %+v", snapshot)
	}
}

func TestRegistrationCommitReleaseRacePreservesAccounting(t *testing.T) {
	cfg := testConfig(&fakeClock{now: time.Unix(1, 0)})
	cfg.MaxConcurrentShares = 1
	cfg.MaxSharesPerSource = 1
	cfg.RegisterPerSource = Rate{PerSecond: 1, Burst: 128}
	c := mustController(t, cfg)
	for i := range 100 {
		r, decision := c.BeginRegister(fmt.Sprintf("source-%d", i))
		if !decision.Allowed() || r.ReserveShare() != Allowed || r.ReserveManifestBytes(1) != Allowed {
			t.Fatalf("stage %d = %v, snapshot=%+v", i, decision, c.Snapshot())
		}
		committed := make(chan *Lease, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			lease, decision := r.Commit(nil)
			if decision.Allowed() {
				committed <- lease
				return
			}
			if decision != InvalidRegistration {
				t.Errorf("commit decision = %v", decision)
			}
			committed <- nil
		})
		wg.Go(r.Release)
		wg.Wait()
		if lease := <-committed; lease != nil {
			lease.Release()
		}
		if snapshot := c.Snapshot(); snapshot.Shares != 0 || snapshot.PendingShares != 0 ||
			snapshot.ManifestBytes != 0 || snapshot.PendingManifestBytes != 0 || snapshot.Sources != 0 {
			t.Fatalf("iteration %d leaked accounting: %+v", i, snapshot)
		}
	}
}

func TestRegistrationReservationRejectsOverflowAndUnownedManifest(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxManifestBytesPerSource = math.MaxInt64
	cfg.MaxTotalManifestBytes = math.MaxInt64
	c := mustController(t, cfg)
	r, decision := c.BeginRegister("source")
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	if decision := r.ReserveManifestBytes(1); decision != InvalidRegistration {
		t.Fatalf("manifest without share reservation = %v", decision)
	}
	if decision := r.ReserveShare(); !decision.Allowed() {
		t.Fatal(decision)
	}
	if decision := r.ReserveManifestBytes(math.MaxInt64 - 1); !decision.Allowed() {
		t.Fatal(decision)
	}
	if decision := r.ReserveManifestBytes(2); decision != ManifestCapacityExceeded {
		t.Fatalf("overflowing reservation = %v", decision)
	}
	r.Release()
	if snapshot := c.Snapshot(); snapshot.PendingManifestBytes != 0 || snapshot.Sources != 0 {
		t.Fatalf("overflow test leaked accounting: %+v", snapshot)
	}
}

func TestRateKeysAreBoundedAndShareRateLivesOnLease(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	cfg := testConfig(clock)
	cfg.RegisterPerSource = Rate{PerSecond: 1, Burst: 1}
	cfg.JoinPerSource = Rate{PerSecond: 1, Burst: 4}
	cfg.JoinPerShare = Rate{PerSecond: 1, Burst: 1}
	c := mustController(t, cfg)

	longSource := strings.Repeat("x", 1<<20)
	r, decision := c.BeginRegister(longSource)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	r.Release()
	for key := range c.register.buckets {
		if len(key) > maxRateKeyBytes {
			t.Fatalf("attacker-sized key retained: %d bytes", len(key))
		}
	}

	share, decision := c.AdmitRegister("owner", 0, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	defer share.Release()
	if decision := allowJoin(c, "source-a", share); !decision.Allowed() {
		t.Fatal(decision)
	}
	for _, source := range []string{"source-b", "source-c", "source-d"} {
		if decision := allowJoin(c, source, share); decision != JoinRateExceeded {
			t.Fatalf("share limiter was reset for %s: %v", source, decision)
		}
	}
}

func TestInvalidRetainedLeaseCannotCorruptAccounting(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	a := mustController(t, testConfig(clock))
	b := mustController(t, testConfig(clock))
	lease, decision := a.AdmitRegister("ip-a", 1, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	if _, decision = b.AdmitRegister("ip-b", 1, lease); decision != InvalidRetainedLease {
		t.Fatalf("decision = %v", decision)
	}
	if got := b.Snapshot().Shares; got != 0 {
		t.Fatalf("shares = %d", got)
	}
	lease.Release()
}

func TestDepletedSourceBucketsAreNotResetByChurn(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	cfg := testConfig(clock)
	cfg.JoinPerSource = Rate{PerSecond: 1, Burst: 1}
	c := mustController(t, cfg)
	for i := range maxTrackedRateKeys {
		if _, decision := c.BeginJoin(fmt.Sprintf("source-%d", i)); !decision.Allowed() {
			t.Fatalf("initial source %d denied: %v", i, decision)
		}
	}
	if _, decision := c.BeginJoin("new-source"); decision != JoinRateExceeded {
		t.Fatalf("active table admitted a resettable key: %v", decision)
	}
	if _, decision := c.BeginJoin("source-0"); decision != JoinRateExceeded {
		t.Fatalf("depleted oldest key was reset: %v", decision)
	}
	clock.now = clock.now.Add(time.Second)
	if _, decision := c.BeginJoin("new-source"); !decision.Allowed() {
		t.Fatalf("replenished oldest key was not reclaimable: %v", decision)
	}
	if got := len(c.joinSource.buckets); got != maxTrackedRateKeys {
		t.Fatalf("source table size = %d", got)
	}
}
