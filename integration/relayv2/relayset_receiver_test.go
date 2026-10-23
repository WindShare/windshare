package relayv2_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestReceiverFirstUsableSessionRecoversWithFreeOrReassignedLane(t *testing.T) {
	ctx, trace, urls := startRelaysetScenario(t, 2)
	share := prepareRelaysetShare(t, ctx, trace, urls)
	release := make(chan struct{})
	secondaryStarted := make(chan struct{})
	retrying := make(chan chan struct{}, relaysetEventCapacity)
	connected := make(chan relayset.ReceiverConnection, relaysetEventCapacity)
	var primaryDials, secondaryDials atomic.Int32
	set, err := relayset.NewReceiver(ctx, relayset.ReceiverConfig{
		Clock:    &relaysetClock{now: time.Unix(1, 0)},
		Receiver: liveshare.ReceiverConfig{Capability: share.Capability()},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
			if config.RelayBaseURL == urls[0] {
				primaryDials.Add(1)
			} else {
				resume := release
				if secondaryDials.Add(1) == 1 {
					close(secondaryStarted)
				} else {
					resume = make(chan struct{})
					select {
					case retrying <- resume:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-resume:
				}
			}
			return relayset.DialReceiver(ctx, config)
		},
		Connected: func(connection relayset.ReceiverConnection) { connected <- connection },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	waitRelaysetEvent(t, ctx, secondaryStarted)
	runtime, primary, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.LaneSet().Len() != 1 || waitRelaysetEvent(t, ctx, connected) != primary {
		t.Fatal("blocked candidate prevented the first independent lane")
	}
	initial := share.waitAdmission(t, ctx, primary)
	if initial.Kind != sessionruntime.SenderChannelNewProtocolSession {
		t.Fatal("primary did not establish a new authenticated session")
	}
	sessionID := runtime.ProtocolSessionID()
	trace.RequireRecord(t, "first_relay_ready", testrun.OutcomeSucceeded, initial)

	close(release)
	additional := waitRelaysetEvent(t, ctx, connected)
	attached := share.waitAdmission(t, ctx, additional)
	if additional == primary || attached.Kind != sessionruntime.SenderChannelAttachedLane || runtime.LaneSet().Len() != 2 {
		t.Fatal("additional relay did not join the winning session")
	}

	// Exercise both a lane retirement and an entire WebSocket loss. Holding the
	// replacement dial proves that the healthy lane remains useful during loss.
	for index, failure := range []string{"channel", "websocket"} {
		previous := additional
		oldLane := attached.Lane
		oldDone, live := runtime.LaneDone(oldLane)
		if !live {
			t.Fatalf("additional lane %v was never published", oldLane)
		}
		trace.RequireRecord(t, "relay_loss_injected", testrun.OutcomeSucceeded, struct {
			Failure string                      `json:"failure"`
			Lane    sessionruntime.LaneIdentity `json:"lane"`
		}{failure, oldLane})
		if failure == "channel" {
			err = previous.Channel().Close()
		} else {
			err = previous.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		resume := waitRelaysetEvent(t, ctx, retrying)
		waitRelaysetEvent(t, ctx, oldDone)
		requireRelaysetLive(t, runtime, primary, initial.Lane)
		expectedLanes := 2
		if index == 1 {
			// This is the first catalog request, so a local cache cannot mask a
			// healthy transport that stopped forwarding during secondary recovery.
			root, err := runtime.Catalog().LoadDirectory(ctx, runtime.Descriptor().SyntheticRoot())
			if err != nil || root.EntryCount() != 1 {
				t.Fatalf("healthy relay could not browse during outage: entries=%d err=%v", root.EntryCount(), err)
			}
			// Lane allocation is transport-neutral. An independently attached
			// channel takes the retired slot just as a direct or TURN path would.
			shareID, err := v2.ShareIDFromBytes(share.Registration().ShareID)
			if err != nil {
				t.Fatal(err)
			}
			contender, err := relayset.DialReceiver(ctx, relayv2.ReceiverConfig{RelayBaseURL: urls[0], ShareID: shareID})
			if err != nil {
				t.Fatal(err)
			}
			trace.RequireCleanup(t, "competing receiver", func(context.Context) error { return contender.Close() })
			grant, err := runtime.RequestLane(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.AttachLane(ctx, grant, contender.Channel(), transferlanes.RouteRelay); err != nil {
				t.Fatal(err)
			}
			occupied := share.waitAdmission(t, ctx, contender)
			if occupied.Lane.ID != oldLane.ID || occupied.Lane.Epoch != oldLane.Epoch+1 {
				t.Fatalf("competing channel did not reuse retired slot: previous=%+v occupied=%+v", oldLane, occupied)
			}
			expectedLanes++
		}
		close(resume)
		additional = waitRelaysetEvent(t, ctx, connected)
		attached = share.waitAdmission(t, ctx, additional)
		if attached.Kind != sessionruntime.SenderChannelAttachedLane {
			t.Fatalf("recovery did not attach to the existing session: %+v", attached)
		}
		if index == 0 && (attached.Lane.ID != oldLane.ID || attached.Lane.Epoch != oldLane.Epoch+1) {
			t.Fatalf("recovery did not reuse a free slot: previous=%+v recovered=%+v", oldLane, attached)
		}
		if index == 1 && attached.Lane.ID == oldLane.ID {
			t.Fatalf("recovery displaced the competing channel: %+v", attached)
		}
		if additional == previous || attached.Session == previous.Channel().(interface{ RelaySessionID() v2.RelaySessionID }).RelaySessionID() {
			t.Fatal("recovery reused the retired relay connection")
		}
		if _, live := runtime.LaneDone(oldLane); live {
			t.Fatal("old lane epoch remained admitted")
		}
		requireRelaysetLive(t, runtime, additional, attached.Lane)
		requireRelaysetLive(t, runtime, primary, initial.Lane)
		if runtime.ProtocolSessionID() != sessionID || runtime.LaneSet().Len() != expectedLanes || primaryDials.Load() != 1 || secondaryDials.Load() != int32(index+2) {
			t.Fatalf("recovery disturbed healthy session: primary=%d secondary=%d lanes=%d", primaryDials.Load(), secondaryDials.Load(), runtime.LaneSet().Len())
		}
		trace.RequireRecord(t, "relay_lane_recovered", testrun.OutcomeSucceeded, attached)
	}
	set.Close()
	waitRelaysetEvent(t, ctx, runtime.Done())
	trace.RequireSuccess(t)
}

func TestReceiverInitiallyFailedRelayRecoversWithoutReplacingHealthyGeneration(t *testing.T) {
	const outage = 3 * time.Minute
	ctx, trace, urls := startRelaysetScenario(t, 2)
	share := prepareRelaysetShare(t, ctx, trace, urls)
	started := time.Unix(1, 0)
	clock := &relaysetClock{now: started}
	failed := make(chan struct{})
	connected := make(chan relayset.ReceiverConnection, relaysetEventCapacity)
	var primaryDials, secondaryDials atomic.Int32
	var waiting atomic.Bool
	recovery, err := relayset.NewReceiverRecovery(relayset.ReceiverRecoveryOptions{
		Clock: clock, Jitter: func(delay time.Duration) time.Duration { return delay },
		Observe: func(value relayset.ReceiverRecoveryObservation) {
			if value.Endpoint == urls[1] && value.Phase == relayset.ReceiverRecoveryWaiting {
				waiting.Store(true)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	set, err := recovery.Join(ctx, relayset.ReceiverConfig{
		Receiver: liveshare.ReceiverConfig{Capability: share.Capability()},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
			if config.RelayBaseURL == urls[1] {
				if secondaryDials.Add(1) == 1 {
					close(failed)
				}
				if clock.Now().Before(started.Add(outage)) {
					return nil, &relayv2.RelayError{Code: v2.ErrorNotFound}
				}
			} else {
				primaryDials.Add(1)
				select {
				case <-failed:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return relayset.DialReceiver(ctx, config)
		},
		Connected: func(connection relayset.ReceiverConnection) { connected <- connection },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	runtime, primary, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := runtime.ProtocolSessionID()
	var initial sessionruntime.LaneIdentity
	initial.ID, initial.Epoch = runtime.LaneIdentity()
	for range 2 {
		waitRelaysetEvent(t, ctx, connected)
	}
	requireRelaysetLive(t, runtime, primary, initial)
	if runtime.LaneSet().Len() != 2 || runtime.ProtocolSessionID() != sessionID || primaryDials.Load() != 1 || secondaryDials.Load() < 2 || !waiting.Load() {
		t.Fatalf("recovery disturbed healthy generation: primary=%d secondary=%d lanes=%d slow_wait=%v", primaryDials.Load(), secondaryDials.Load(), runtime.LaneSet().Len(), waiting.Load())
	}
	trace.RequireRecord(t, "failed_candidate_recovered", testrun.OutcomeSucceeded, struct {
		Session  [16]byte `json:"protocol_session_id"`
		Attempts int32    `json:"attempts"`
	}{sessionID, secondaryDials.Load()})
	trace.RequireSuccess(t)
}

func TestReceiverInitialUnavailabilityRecoversWithinJoinWindow(t *testing.T) {
	const initialOutage = time.Second
	ctx, trace, urls := startRelaysetScenario(t, 1)
	share := prepareRelaysetShare(t, ctx, trace, urls)
	started := time.Unix(1, 0)
	clock := &relaysetClock{now: started}
	var attempts, stoppedCalls atomic.Int32
	const stoppedEndpoint = "stopped"
	capability := share.Capability()
	capability.Relays = append(capability.Relays, stoppedEndpoint)
	recovery, err := relayset.NewReceiverRecovery(relayset.ReceiverRecoveryOptions{
		Clock: clock, Jitter: func(delay time.Duration) time.Duration { return delay },
	})
	if err != nil {
		t.Fatal(err)
	}
	config := relayset.ReceiverConfig{
		Receiver: liveshare.ReceiverConfig{Capability: capability},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
			if config.RelayBaseURL == stoppedEndpoint {
				stoppedCalls.Add(1)
				return nil, &relayv2.RelayError{Code: v2.ErrorStopped}
			}
			attempts.Add(1)
			if clock.Now().Before(started.Add(initialOutage)) {
				return nil, &relayv2.RelayError{Code: v2.ErrorNotFound}
			}
			return relayset.DialReceiver(ctx, config)
		},
	}
	set, err := recovery.Join(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	original, _, err := set.WaitReady(ctx)
	if err != nil || attempts.Load() < 2 {
		t.Fatalf("initial join did not recover: attempts=%d err=%v", attempts.Load(), err)
	}
	set.Close()
	// A successful generation must not erase a different relay's permanent
	// rejection when the share owner creates its next protocol session.
	next, err := recovery.Replace(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	replacement, _, err := next.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ProtocolSessionID() == original.ProtocolSessionID() || stoppedCalls.Load() != 1 {
		t.Fatalf("replacement reset endpoint decisions: stopped_calls=%d", stoppedCalls.Load())
	}
	trace.RequireRecord(t, "initial_outage_recovered", testrun.OutcomeSucceeded, struct {
		Attempts     int32 `json:"attempts"`
		StoppedCalls int32 `json:"stopped_calls"`
	}{attempts.Load(), stoppedCalls.Load()})
	trace.RequireSuccess(t)
}
