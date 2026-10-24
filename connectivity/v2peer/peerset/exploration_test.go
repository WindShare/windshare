package peerset

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestExplorationUsesOneAttemptAndLeavesRecoveryBudgetUntouched(t *testing.T) {
	clock := newClock()
	owner, _ := newTestPeerSet(t, Config{Clock: clock})
	before := owner.config.Budget.attempts
	calls := make(chan v2signal.Binding, 2)
	path, err := owner.Open(t.Context(), PathConfig{Demand: ContentDemand, Purpose: v2signal.ThroughputExploration,
		Start: func(_ context.Context, binding v2signal.Binding) (Attempt, error) {
			calls <- binding
			return nil, errors.New("optional unavailable")
		}})
	if err != nil {
		t.Fatal(err)
	}
	binding := receive(t, calls)
	if binding.Purpose != v2signal.ThroughputExploration {
		t.Fatal("purpose lost")
	}
	receive(t, path.Done())
	if owner.config.Budget.attempts != before {
		t.Fatal("exploration consumed the shared recovery budget")
	}
	clock.advance(RefillWindow)
	select {
	case <-calls:
		t.Fatal("optional path retried")
	default:
	}
}

func TestPathUnavailableIsAggregatedBeforeEndingSession(t *testing.T) {
	owner, _ := newTestPeerSet(t, Config{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &ReceiverSession{owner: owner, ctx: ctx, cancel: cancel}
	owner.paths[pathKey{}] = &Path{}
	s.pathFinished(Result{Cause: ErrWaveExhausted})
	if ctx.Err() != nil {
		t.Fatal("one path canceled another path's recovery")
	}
	s.pathFinished(Result{Scope: protocolsession.PeerFailureSessionTerminal, Cause: errors.New("authentication")})
	if ctx.Err() == nil {
		t.Fatal("authentication failure lost session meaning")
	}
}
