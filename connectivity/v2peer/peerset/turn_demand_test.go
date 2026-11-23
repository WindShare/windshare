package peerset

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

func turnPressure(delivered uint64) lanes.SchedulingDemand {
	return lanes.SchedulingDemand{NetworkWaitingBytes: 1, RemainingBytes: 32 << 20, UsefulBytes: delivered}
}

func TestTURNRenewalRequestsFreshCredentialsWhenNetworkWorkResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		f.runtime.attach(session.LaneIdentity{ID: 100})
		grant := turnGrant("home")
		grant.ValidForMS = 60_000
		f.manager.acceptService(f.origin, grant, false)
		synctest.Wait()
		first := receive(t, f.attempts)
		f.admit(first)
		tail := lanes.SchedulingDemand{InFlightBytes: 4096, UnfinishedBytes: 4096, RemainingBytes: 4096}
		f.step(44*time.Second, tail)
		f.step(45*time.Second, lanes.SchedulingDemand{CapacityWaitingBytes: 4096})
		if len(f.runtime.requests) != 0 {
			t.Fatal("requested an early or sink-bound renewal")
		}
		f.step(46*time.Second, tail)
		f.reply(f.request(p.TURNAll, "home"), p.TURNOK, "home")
		f.step(46*time.Second, tail)
		renewed := receive(t, f.attempts)
		if renewed.binding.PeerPathID != first.binding.PeerPathID || renewed.binding.AttemptSequence != 2 {
			t.Fatal("normal renewal abandoned the service's established binding")
		}
	})
}

func TestFullTURNRequestEscalationWaitsOnlyForTheOutstandingRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		d := turnPressure(0)
		f.step(0, d)
		parallel := f.request(p.TURNParallel, "")
		f.step(3*time.Second, d)
		if len(f.runtime.requests) != 0 {
			t.Fatal("parallel and all requests were outstanding together")
		}
		f.reply(parallel, p.TURNRetryLater)
		f.step(3*time.Second, d)
		all := f.request(p.TURNAll, "")
		if all.RequestID == parallel.RequestID {
			t.Fatal("stronger request reused the outstanding request ID")
		}
	})
}

func TestEscalationCanRecoverAfterParallelExhaustsItsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		for _, second := range []int{0, 2, 12, 42} {
			elapsed := time.Duration(second) * time.Second
			fast := turnPressure(uint64(second) * (2 << 20))
			f.step(elapsed, fast)
			f.reply(f.request(p.TURNParallel, ""), p.TURNRetryLater)
			f.step(elapsed, fast)
		}
		f.step(44*time.Second, turnPressure(88<<20))
		if len(f.runtime.requests) != 0 {
			t.Fatal("parallel demand exceeded its bounded retries")
		}
		f.step(47*time.Second, turnPressure(88<<20))
		all := f.request(p.TURNAll, "")
		f.reply(all, p.TURNRetryLater)
		f.step(47*time.Second, turnPressure(88<<20))
		f.step(49*time.Second, turnPressure(88<<20))
		f.request(p.TURNAll, "")
	})
}

func TestPartialTURNGrantsAdmitAvailableServicesAndBoundMissingRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		d := turnPressure(0)
		f.step(0, d)
		f.reply(f.request(p.TURNParallel, ""), p.TURNOK, "parallel")
		f.step(0, d)
		parallel := receive(t, f.attempts)
		f.admit(parallel)

		f.step(3*time.Second, d)
		f.reply(f.request(p.TURNAll, ""), p.TURNPartialRetryable, "parallel", "first-fallback")
		f.step(3*time.Second, d)
		fallback := receive(t, f.attempts)
		if fallback.grant.ServiceID != "first-fallback" || len(f.attempts) != 0 {
			t.Fatal("partial reply restarted a retained service or withheld a fresh one")
		}
		f.admit(fallback)
		// Pressure weakens before backoff ends. The outstanding all budget can
		// resume only when aggregate delivery needs fallback again.
		d.UsefulBytes = 16 << 20
		f.step(5*time.Second, d)
		if len(f.runtime.requests) != 0 {
			t.Fatal("partial fallback retry spent quota on a fast relay")
		}
		for _, second := range []int{8, 18, 48} {
			elapsed := time.Duration(second) * time.Second
			f.step(elapsed-time.Millisecond, d)
			if len(f.runtime.requests) != 0 {
				t.Fatal("repeated partial reply reset or skipped backoff")
			}
			f.step(elapsed, d)
			f.reply(f.request(p.TURNAll, ""), p.TURNPartialRetryable, "parallel", "first-fallback")
			f.step(elapsed, d)
			if len(f.attempts) != 0 {
				t.Fatal("a retained grant restarted its healthy path")
			}
		}
		f.step(90*time.Second, d)
		if len(f.runtime.requests) != 0 {
			t.Fatal("repeated partial responses bypassed the full-request budget")
		}
	})
}

func TestExhaustedTURNServiceFreesItsPlaceForNewlyDiscoveredFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		f.step(0, turnPressure(0))
		f.reply(f.request(p.TURNParallel, ""), p.TURNOK, "dead", "a", "b", "c")
		f.step(0, turnPressure(0))
		var dead *testTURNAttempt
		for range p.MaxGrantsPerMessage {
			a := receive(t, f.attempts)
			if a.grant.ServiceID == "dead" {
				dead = a
				a.Close()
			} else {
				f.admit(a)
			}
		}
		synctest.Wait()
		f.step(0, turnPressure(0))
		for _, second := range []int{2, 12, 42} {
			elapsed := time.Duration(second) * time.Second
			fast := turnPressure(uint64(second) * (2 << 20))
			f.step(elapsed, fast)
			f.reply(f.request(p.TURNAll, "dead"), p.TURNRetryLater)
			f.step(elapsed, fast)
		}
		if len(f.manager.services) != 3 {
			t.Fatal("exhausted service still occupied a place")
		}
		f.step(45*time.Second, turnPressure(84<<20))
		f.reply(f.request(p.TURNAll, ""), p.TURNPartialRetryable, "dead", "a", "b", "new-fallback")
		f.step(45*time.Second, turnPressure(84<<20))
		next := receive(t, f.attempts)
		if next.grant.ServiceID != "new-fallback" || len(f.attempts) != 0 || len(f.manager.services) != 4 {
			t.Fatal("a duplicate resurrected the dead service or displaced a healthy one")
		}
		if next.binding.PeerSlot != dead.binding.PeerSlot || next.binding.PeerPathID != dead.binding.PeerPathID ||
			next.binding.AttemptSequence != dead.binding.AttemptSequence+1 {
			t.Fatal("released slot did not retain its path identity and sequence")
		}
		f.admit(next)
		f.step(90*time.Second, turnPressure(84<<20))
		if len(f.runtime.requests) != 0 {
			t.Fatal("partial response retried with four services already active")
		}
	})
}
