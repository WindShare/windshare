package v2endpoint

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestForwardPressureWaitsForCapacityWithoutRetiringDestination(t *testing.T) {
	source, destination, id, frame := pressureFixture(t)
	events := make(chan ForwardTrace, 4)
	server := &Server{writeTimeout: time.Second, forwardTracer: ForwardTraceFunc(func(e ForwardTrace) { events <- e })}
	ctx := t.Context()
	result := make(chan error, 1)
	go func() { result <- server.forwardWithPressure(ctx, source, destination, id, frame) }()
	event := receivePressureEvent(t, events)
	if event.Stage != "queue_wait" || event.SessionFrames != MaximumSessionQueueFrames ||
		event.Source != source.ref || event.Destination != destination.ref || event.SessionID != id {
		t.Fatalf("pressure evidence = %+v", event)
	}
	select {
	case err := <-result:
		t.Fatalf("full live queue settled before consumption: %v", err)
	default:
	}
	if destination.closed.Load() {
		t.Fatal("transient pressure closed the destination")
	}
	if _, ok := destination.takeForward(); !ok {
		t.Fatal("writer could not consume the full queue")
	}
	if err := receivePressureResult(t, result); err != nil {
		t.Fatal(err)
	}
	if event = receivePressureEvent(t, events); event.Stage != "queue_resumed" {
		t.Fatalf("resumption evidence = %+v", event)
	}
	for range MaximumSessionQueueFrames {
		queued, ok := destination.takeForward()
		if !ok || !bytes.Equal(queued, frame) {
			t.Fatal("pressure changed queued frame order or contents")
		}
	}
	if _, ok := destination.takeForward(); ok {
		t.Fatal("pressure duplicated a frame")
	}
}

func TestForwardPressureCancellationAndRetirementWakeWaiters(t *testing.T) {
	for _, end := range []string{"cancel", "remove-session", "close-destination"} {
		t.Run(end, func(t *testing.T) {
			source, destination, id, frame := pressureFixture(t)
			events := make(chan ForwardTrace, 4)
			server := &Server{writeTimeout: time.Second, forwardTracer: ForwardTraceFunc(func(e ForwardTrace) { events <- e })}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- server.forwardWithPressure(ctx, source, destination, id, frame) }()
			receivePressureEvent(t, events)
			want := ErrConnection
			switch end {
			case "cancel":
				want = context.Canceled
				cancel()
			case "remove-session":
				destination.removeSession(id)
			case "close-destination":
				destination.requestClose()
			}
			if err := receivePressureResult(t, result); !errors.Is(err, want) {
				t.Fatalf("settlement = %v, want %v", err, want)
			}
		})
	}
}

func TestForwardPressureDeadlineIsBoundedAndDiagnosed(t *testing.T) {
	source, destination, id, frame := pressureFixture(t)
	events := make(chan ForwardTrace, 4)
	server := &Server{writeTimeout: time.Millisecond, forwardTracer: ForwardTraceFunc(func(e ForwardTrace) { events <- e })}
	err := server.forwardWithPressure(context.Background(), source, destination, id, frame)
	if !errors.Is(err, ErrForwardTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled queue = %v", err)
	}
	receivePressureEvent(t, events)
	if event := receivePressureEvent(t, events); event.Stage != "queue_wait_expired" || event.Wait <= 0 {
		t.Fatalf("deadline evidence = %+v", event)
	}
}

func TestForwardCapacityWakeupIsNotLostAcrossMultipleWaiters(t *testing.T) {
	_, destination, id, frame := pressureFixture(t)
	_, first, _ := destination.tryForward(id, frame)
	_, second, _ := destination.tryForward(id, frame)
	if first == nil || first != second {
		t.Fatal("waiters did not subscribe to the same capacity generation")
	}
	destination.takeForward()
	for _, changed := range []<-chan struct{}{first, second} {
		select {
		case <-changed:
		default:
			t.Fatal("capacity release did not broadcast to every waiter")
		}
	}
	if !destination.enqueueForward(id, frame) {
		t.Fatal("released capacity was unavailable")
	}
	_, next, _ := destination.tryForward(id, frame)
	if next == first {
		t.Fatal("a new saturation reused an already-signalled generation")
	}
}

func pressureFixture(t *testing.T) (*connection, *connection, v2.RelaySessionID, []byte) {
	t.Helper()
	id := relaySessionIDForEndpointTest(99)
	source := newEndpointTestConnection("pressure-source", nil, func() {})
	destination := newEndpointTestConnection("pressure-destination", nil, func() {})
	destination.addSession(id)
	frame := []byte("bounded frame")
	for range MaximumSessionQueueFrames {
		if !destination.enqueueForward(id, frame) {
			t.Fatal("could not fill queue")
		}
	}
	return source, destination, id, frame
}

func receivePressureEvent(t *testing.T, events <-chan ForwardTrace) ForwardTrace {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("forwarding pressure event did not arrive")
		return ForwardTrace{}
	}
}

func receivePressureResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(time.Second):
		t.Fatal("forwarding waiter did not settle")
		return nil
	}
}

func (peer *connection) enqueueForward(sessionID v2.RelaySessionID, encoded []byte) bool {
	accepted, _, _ := peer.tryForward(sessionID, encoded)
	return accepted
}
