package turn

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/core/session/turncontrol"
)

func nextControl[T turncontrol.Message](t *testing.T, messages <-chan turncontrol.Message) T {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-messages:
			if value, ok := message.(T); ok {
				return value
			}
		case <-deadline.C:
			t.Fatal("TURN control did not arrive")
		}
	}
}

func TestRunningSenderDiscoversConfigurationAndRetainsExistingLane(t *testing.T) {
	m := newConfiguredManager(t, accountConfig{})
	messages := make(chan turncontrol.Message, 32)
	promoted := make(chan struct{}, 1)
	sender := turnpath.NewSender(turnpath.SenderConfig{Accounts: m, SessionID: [16]byte{1},
		Send: func(_ context.Context, body []byte) error {
			message, err := turncontrol.Decode(body)
			if err == nil {
				messages <- message
			}
			return err
		},
		Observe: func(event turnpath.Event) {
			if event.Milestone == "replacement_admitted" {
				promoted <- struct{}{}
			}
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); sender.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("sender shutdown did not join")
		}
	})
	config := configurationFor(newVersionedSource("issuer"), 100)
	reconfigure(t, m, config)
	available := nextControl[turncontrol.Available](t, messages)
	if len(available.Profiles) != 1 || available.Profiles[0] != "primary" {
		t.Fatal(available)
	}
	demand := available.Authority
	demand.Sequence = 1
	body, _ := turncontrol.Encode(turncontrol.Demand{Authority: demand, HoldFor: time.Minute})
	if err := sender.Handle(body); err != nil {
		t.Fatal(err)
	}
	grant := nextControl[turncontrol.Grant](t, messages)
	config.Accounts[0].LocalBudgetBytes = 200
	reconfigure(t, m, config)
	pendingUpdate := nextControl[turncontrol.Available](t, messages)
	if pendingUpdate.Authority.Sequence <= available.Authority.Sequence {
		t.Fatal("missing settings revision")
	}
	if authorized, _, err := sender.Lookup(grant.PathID); err != nil || authorized == nil {
		t.Fatal("ordinary settings update retired an already delegated pending path")
	}
	sender.Admitted(grant.PathID)
	body, _ = turncontrol.Encode(turncontrol.Ready{Authority: grant.Authority, PathID: grant.PathID})
	if err := sender.Handle(body); err != nil {
		t.Fatal(err)
	}
	<-promoted
	config.Profiles = append(config.Profiles, profile{ID: "secondary", AccountID: "owner", URLs: []string{"turn:localhost:3479"}})
	reconfigure(t, m, config)
	next := nextControl[turncontrol.Available](t, messages)
	if next.Authority.OwnerPathID != available.Authority.OwnerPathID || next.Authority.Sequence <= available.Authority.Sequence || len(next.Profiles) != 2 {
		t.Fatal("configuration update lost owner identity or inventory revision")
	}
	second := nextControl[turncontrol.Grant](t, messages)
	if second.Profile != "secondary" {
		t.Fatal("new profile did not join the active demand")
	}
	if authorized, _, err := sender.Lookup(grant.PathID); err != nil || authorized == nil {
		t.Fatal("unchanged service lost its current lane")
	}
	reconfigure(t, m, accountConfig{})
	empty := nextControl[turncontrol.Available](t, messages)
	if len(empty.Profiles) != 0 {
		t.Fatal("removing settings did not withdraw advertised services")
	}
	if _, _, err := sender.Lookup(grant.PathID); err == nil {
		t.Fatal("removed service retained lookup authority")
	}
}
