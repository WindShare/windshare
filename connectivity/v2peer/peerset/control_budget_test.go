package peerset

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
)

func TestRepeatedMappingAndRemoteNetworkNoticesSpendOnlyCurrentWave(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	native := nativepeer.New(nativepeer.Config{Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond), Now: clock.Now, Reachability: reachability.New(reachability.Config{Now: clock.Now})})
	defer native.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := &Path{owner: owner, config: PathConfig{Native: native, StopAfterWave: true}, key: pathKey{path: v2signal.PeerPathID{3}}, ctx: ctx, cancel: cancel, demand: ContentDemand, wake: make(chan struct{}, 1), resourceChanges: make(chan struct{}, 1)}
	wave := recoveryWave{started: clock.Now()}
	started := wave.started
	for attempt := 0; attempt < AttemptsPerWave; attempt++ {
		if attempt > 0 {
			for range 12 {
				path.handleNativeChange(ctx, nativepeer.Change{Remote: true, MappingReady: true, RemoteNetworkChanged: true})
			}
			if path.consumeRestart() {
				t.Fatal("remote notice interrupted live ICE")
			}
		}
		opportunity, stop := path.prepareOpportunity(&wave)
		if stop != nil || opportunity == nil {
			t.Fatal(opportunity, stop)
		}
		wave.attempts++
		opportunity.refund(0)
		opportunity.release()
		if wave.started != started {
			t.Fatal("control reset original wave")
		}
		clock.advance(time.Second)
	}
	if !wave.exhausted(clock.Now()) {
		t.Fatal("notices enlarged attempt allowance")
	}
	if stop := path.nextWave(&wave); stop == nil || stop.Cause != ErrWaveExhausted {
		t.Fatal("p2p-only control created a second wave", stop)
	}
	owner.config.Budget.mu.Lock()
	remaining := owner.config.Budget.attempts
	owner.config.Budget.mu.Unlock()
	if remaining >= 6 || remaining < 5 {
		t.Fatal("mapping opportunity escaped intent accounting", remaining)
	}
}
func TestExpiredPrewarmDormantOwnerTakesContentWithoutAnotherSpeculativeAttempt(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	attempts := make(chan *fakeAttempt, 3)
	path, err := owner.Open(context.Background(), PathConfig{Demand: BrowseDemand, Start: func(context.Context, v2signal.Binding) (Attempt, error) {
		a := newAttempt()
		attempts <- a
		return a, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer path.Close()
	first := receive(t, attempts)
	nextTimer(t, clock, AttemptBudget)
	close(first.ready)
	receive(t, path.Ready())
	retention := nextTimer(t, clock, PrewarmRetention)
	clock.advance(PrewarmRetention)
	retention.fire(clock.Now())
	receive(t, first.Done())
	select {
	case <-attempts:
		t.Fatal("browse retried")
	default:
	}
	if err := path.SetDemand(ContentDemand); err != nil {
		t.Fatal(err)
	}
	second := receive(t, attempts)
	if second == first {
		t.Fatal("expired provider reused")
	}
	nextTimer(t, clock, AttemptBudget)
	owner.config.Budget.mu.Lock()
	remaining := owner.config.Budget.attempts
	owner.config.Budget.mu.Unlock()
	if remaining >= 7 {
		t.Fatal("content takeover replenished intent attempt budget", remaining)
	}
}
