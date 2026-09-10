package v2route

import (
	"context"
	"testing"
	"time"
)

func TestStopTraceExplainsCapacityDispositionAndAllowsReentry(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome CommitOutcome
		want    string
		routes  int
	}{
		{name: "commit", outcome: CommitCommitted, want: "committed", routes: 0},
		{name: "failure", outcome: CommitNotCommitted, want: "failed", routes: 1},
		{name: "uncertain", outcome: CommitUnknown, want: "uncertain", routes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			store := newBlockingCommitStore()
			registry := newRegistry(t, &now, store, 1)
			fixture := makeFixture(t, 11)
			publishRoute(t, registry, fixture, routeTestConnection("sender"))
			var events []StopTrace
			registry.stopTracer = StopTraceFunc(func(event StopTrace) {
				// An observer can inspect the completed lifecycle without
				// running beneath the lock that made the transition.
				_, _ = registry.Join(fixture.init.ShareID, routeTestConnection("observer"))
				events = append(events, event)
			})
			var commitErr error
			if test.outcome != CommitCommitted {
				commitErr = errInjectedTombstone
			}
			store.replies <- commitReply{outcome: test.outcome, err: commitErr}
			_, _ = registry.Stop(context.Background(), fixture.stop, fixture.stopAuth)
			if len(events) != 1 {
				t.Fatalf("STOP trace events = %d", len(events))
			}
			event := events[0]
			if event.ShareID != fixture.init.ShareID || event.StopID != fixture.stop.StopID ||
				event.Outcome != test.want || event.ActiveRoutes != test.routes || event.RouteCapacity != 1 {
				t.Fatalf("STOP trace = %+v", event)
			}
			if (event.Err == nil) != (commitErr == nil) {
				t.Fatalf("STOP trace lost failure: %v", event.Err)
			}
		})
	}
}
