package networkcommand

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/internal/platformsetup"
)

type checkFunc func(context.Context) platformsetup.Report

func (check checkFunc) Check(ctx context.Context) platformsetup.Report { return check(ctx) }

type eventChannel chan clievent.Event

func (events eventChannel) Observe(event clievent.Event) bool { events <- event; return true }

func TestBackgroundCheckIsBoundedAndJoinsOnCancellation(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	checker := checkFunc(func(ctx context.Context) platformsetup.Report {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > platformsetup.CheckTimeout {
			t.Error("missing check deadline")
		}
		close(started)
		<-ctx.Done()
		close(finished)
		return platformsetup.Report{}
	})
	stop := StartCheck(t.Context(), checker, eventChannel(make(chan clievent.Event, 1)), clievent.CommandShare)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background check did not start")
	}
	stop()
	select {
	case <-finished:
	default:
		t.Fatal("check outlived command")
	}
	StartCheck(t.Context(), nil, nil, clievent.CommandGet)()
}

func TestBackgroundRecommendationIsOnceAndHonorsSavedChoice(t *testing.T) {
	for _, choice := range []platformsetup.Choice{platformsetup.Ask, platformsetup.Skip, platformsetup.Configure} {
		for _, reason := range []platformsetup.Reason{platformsetup.DefaultBlock, platformsetup.ExplicitBlock, platformsetup.ReadFailed} {
			events := make(eventChannel, 2)
			checker := checkFunc(func(context.Context) platformsetup.Report {
				return platformsetup.Report{Choice: choice, Findings: []platformsetup.Finding{
					{Protocol: platformsetup.UDP, Verdict: platformsetup.Blocked, Reason: reason},
					{Protocol: platformsetup.TCP, Verdict: platformsetup.Blocked, Reason: reason},
				}}
			})
			stop := StartCheck(t.Context(), checker, events, clievent.CommandGet)
			suggestions := 0
			for range 2 {
				select {
				case value := <-events:
					if value.(clievent.NetworkPolicyObserved).SuggestSetup() {
						suggestions++
					}
				case <-time.After(time.Second):
					stop()
					t.Fatal("policy evidence was not published")
				}
			}
			stop()
			want := 0
			if choice != platformsetup.Skip && reason == platformsetup.DefaultBlock {
				want = 1
			}
			if suggestions != want {
				t.Fatal(choice, reason, suggestions)
			}
		}
	}
}
