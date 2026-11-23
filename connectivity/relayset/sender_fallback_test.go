package relayset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/connectivity/relayplan"
)

func TestCanaryStartsAfterEveryActiveFailsAndKeepsPendingRoutes(t *testing.T) {
	primary := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	reject := make(chan struct{})
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "retrying"}, {URL: "rejected"}}, Standby: []relayplan.Candidate{{URL: "standby", Canary: true}}}
	standby := make(chan struct{})
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "retrying":
			return primary, nil
		case "rejected":
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-reject:
				return nil, errors.New("registration denied")
			}
		default:
			close(standby)
			return newEndpoint(), nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, primary.observed)
	primary.publish(RegistrationRetrying)
	if got := set.Registrations().Candidates; !slices.Equal(got, []string{"rejected", "retrying"}) {
		t.Fatalf("premature fallback: %v", got)
	}
	close(reject)
	waitValue(t, standby)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := set.Registrations()
	if set.ReadyRelayURL() != "standby" || !slices.Equal(got.Candidates, []string{"retrying", "standby"}) || !slices.Equal(got.Rejected, []string{"rejected"}) {
		t.Fatal(set.ReadyRelayURL(), got)
	}
}

func TestCanaryCannotStartAfterPublication(t *testing.T) {
	active := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "active"}}, Standby: []relayplan.Candidate{{URL: "dormant", Canary: true}}}
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		if url != "active" {
			t.Errorf("dialed dormant relay %s", url)
		}
		return active, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, active.observed)
	active.publish(RegistrationAvailable)
	close(active.ready)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	active.publish(RegistrationRetrying)
	set.Wake()
	if got := set.Registrations().Candidates; !slices.Equal(got, []string{"active"}) {
		t.Fatal(got)
	}
}

func TestCanaryReusesLastLinkSlotWithoutExceedingLimit(t *testing.T) {
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "active"}}, Standby: []relayplan.Candidate{{URL: "canary", Canary: true}}}
	for i := range MaximumEndpoints - 1 {
		plan.Configured = append(plan.Configured, relayplan.Candidate{URL: fmt.Sprint(i)})
	}
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "active":
			return nil, errors.New("refused")
		case "canary":
			return newEndpoint(), nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := set.Registrations().Candidates
	if len(got) != MaximumEndpoints || slices.Contains(got, "active") || !slices.Contains(got, "canary") {
		t.Fatal(got)
	}
}
