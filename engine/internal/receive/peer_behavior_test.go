package receive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

func TestReceiverPeerDispositionOwnsSessionClosure(t *testing.T) {
	retained := errors.Join(context.Canceled, errors.New("peer shutdown failed"))
	for _, test := range []struct {
		name        string
		disposition receiverPeerDisposition
		signal      receiverPeerSignal
		closed      int32
		local       bool
		warning     bool
	}{
		{"path failure", receiverPeerFallbackAllowed, receiverPeerFailed, 0, false, false},
		{"session unavailable", receiverPeerSessionUnavailable, receiverPeerRuntimeTerminal, 0, false, true},
		{"session unsafe", receiverPeerSessionUnsafe, receiverPeerSessionFatal, 1, false, true},
		{"local stop with residue", receiverPeerLocalStop, 0, 0, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation, events := newTestObservation(t)
			attempt := newCLIReceiverPeerAttempt()
			runtime := &testRuntimeCloser{}
			attempt.finishOutcome(NewPeerOutcome(test.disposition, retained))
			var signals []receiverPeerSignal
			local := (&runner{}).monitorReceiverPeer(attempt, runtime, protocolsession.ProtocolSessionID{1}, observation, func(signal receiverPeerSignal) { signals = append(signals, signal) })
			if local != test.local || runtime.calls.Load() != test.closed {
				t.Fatalf("local=%v closes=%d", local, runtime.calls.Load())
			}
			if test.signal == 0 {
				if len(signals) != 0 {
					t.Fatal(signals)
				}
			} else if len(signals) != 1 || signals[0] != test.signal {
				t.Fatal(signals)
			}
			warnings := warningEvents(*events)
			if (len(warnings) > 0) != test.warning {
				t.Fatalf("warnings=%v", warnings)
			}
			if test.warning && !errors.Is(warnings[0].(Warning).Cause, retained) {
				t.Fatalf("joined failure=%v", warnings[0])
			}
		})
	}
}
func TestReceiverPeerReadyDetachAndCloseJoin(t *testing.T) {
	observation, events := newTestObservation(t)
	attempt := newCLIReceiverPeerAttempt()
	attempt.lane = sessionruntime.LaneIdentity{ID: 2, Epoch: 1}
	signals := make(chan receiverPeerSignal, 2)
	done := make(chan struct{})
	stop := &receiverLocalStop{}
	peer := &activeReceiverPeer{attempt: attempt, done: done, localStop: stop}
	go func() {
		defer close(done)
		(&runner{}).monitorReceiverPeer(attempt, &testRuntimeCloser{}, protocolsession.ProtocolSessionID{1}, observation, func(signal receiverPeerSignal) { signals <- signal })
	}()
	close(attempt.ready)
	if signal := <-signals; signal != receiverPeerReady {
		t.Fatal(signal)
	}
	attempt.finish(errors.New("direct path lost"))
	if signal := <-signals; signal != receiverPeerDetached {
		t.Fatal(signal)
	}
	peer.CloseWithReason(ReceiverLocalStopNormalCompletion)
	peer.Close()
	if stop.snapshot() != ReceiverLocalStopNormalCompletion {
		t.Fatal(stop.snapshot())
	}
	adopted := 0
	for _, event := range *events {
		if _, ok := event.(LaneAdopted); ok {
			adopted++
		}
	}
	if adopted != 1 {
		t.Fatalf("adopted=%d", adopted)
	}
}
func TestReceiverContentPathsDescribeUsefulDeliveryAndRealFallback(t *testing.T) {
	observation, events := newTestObservation(t)
	paths := newReceiverContentPaths(observation)
	now := time.Unix(100, 0)
	direct := transfer.LaneContentActivity{Route: transfer.LaneRouteDirect, AdmittedLanes: 1, UsefulBytes: 10, LastUsefulAt: now}
	relay := transfer.LaneContentActivity{Route: transfer.LaneRouteRelay, AdmittedLanes: 1, UsefulBytes: 10, LastUsefulAt: now}
	paths.observeContent([]transfer.LaneContentActivity{direct}, now)
	paths.observeContent([]transfer.LaneContentActivity{direct, relay}, now)
	paths.observeContent([]transfer.LaneContentActivity{relay}, now)
	var selected []ContentPath
	fallbacks := 0
	for _, event := range *events {
		switch value := event.(type) {
		case ContentPathObserved:
			selected = append(selected, value.Path)
		case FallbackObserved:
			fallbacks++
		}
	}
	if len(selected) != 3 || selected[0] != ContentPathDirect || selected[1] != ContentPathDirectAndRelay || selected[2] != ContentPathRelay || fallbacks != 1 {
		t.Fatalf("paths=%v fallback=%d", selected, fallbacks)
	}
	// Idle but admitted peers carry no useful traffic; that cannot prove fallback.
	observation, events = newTestObservation(t)
	paths = newReceiverContentPaths(observation)
	direct.UsefulBytes = 0
	direct.LastUsefulAt = time.Time{}
	paths.observeContent([]transfer.LaneContentActivity{direct, relay}, now)
	paths.observeContent([]transfer.LaneContentActivity{relay}, now)
	for _, event := range *events {
		if _, ok := event.(FallbackObserved); ok {
			t.Fatal("idle peer invented fallback")
		}
	}
}
func TestReceiverPeerFactoryFailurePreservesRequiredAdmission(t *testing.T) {
	observation, events := newTestObservation(t)
	worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) { return nil, errors.New("configuration failed") }}
	var signals []receiverPeerSignal
	peer := worker.startReceiverPeer(context.Background(), nil, observation, func(signal receiverPeerSignal) { signals = append(signals, signal) }, &receiverLocalStop{}, receiverPeerRequired)
	if peer != nil || len(signals) != 1 || signals[0] != receiverPeerFailed {
		t.Fatalf("peer=%v signals=%v", peer, signals)
	}
	if warnings := warningEvents(*events); len(warnings) != 1 || warnings[0].(Warning).Code != FailurePeerConfiguration {
		t.Fatalf("warnings=%v", warnings)
	}
}
