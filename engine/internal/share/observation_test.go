package share

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"

	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/engine/internal/task"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestShareProtocolAndTerminalObservationAuthorityFollowsRequest(t *testing.T) {
	for _, test := range []struct {
		name    string
		request Request
	}{
		{"default", Request{}},
		{"diagnostics", Request{Diagnostics: testDiagnosticSink(t)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newShareFixture(t)
			o := newObservations(f.dependencies.Control, test.request)
			config := o.runtimeConfig(f.relays, nil)
			if !config.ProtocolObservations.IsZero() != test.request.Diagnostics.Enabled() {
				t.Fatal("protocol producer ignored diagnostic authority")
			}
			if (config.TerminalSendObserver != nil) != test.request.Diagnostics.Enabled() || (config.SessionTerminalObserver != nil) != test.request.Diagnostics.Enabled() {
				t.Fatal("terminal observers ignored lifecycle trace authority")
			}
			o.complete()
		})
	}
}

func TestShareFinalObservationCutWaitsForRuntimeAndPreservesUpstreamLoss(t *testing.T) {
	f := newShareFixture(t)
	f.factory.entered, f.factory.release = make(chan struct{}), make(chan struct{})
	var producer observationstream.Producer[sessionobs.ProtocolObservation]
	f.prepared.observeRuntime = func(config liveshare.RuntimeFactoryConfig) { producer = config.ProtocolObservations }
	f.factory.observeStop = func() {
		producer.TryPublish(sessionobs.ProtocolOperationObservation{})
		producer.RecordDropped(2)
	}
	ctx, stop := context.WithCancelCause(context.Background())
	stream, _ := diagnostics.NewStream(256, nil)
	done := fixtureRun(f, ctx, Request{Diagnostics: stream.Sink()})
	if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop(task.ErrShareStopped)
	<-f.factory.entered
	if !producer.TryPublish(sessionobs.ProtocolOperationObservation{}) {
		t.Fatal("runtime observation authority was cut before join")
	}
	close(f.factory.release)
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil {
		t.Fatalf("result=%+v", result)
	}
	if producer.TryPublish(sessionobs.ProtocolOperationObservation{}) {
		t.Fatal("completed share retained observation admission")
	}
	var protocols int
	var lost uint64
	cut := stream.Close()
	for record := range stream.Records() {
		if record.Name() == "protocol_operation" {
			protocols++
		}
		if record.Name() == "diagnostic_loss" && record.Payload().Get("source").Text() == "protocol" {
			lost += record.Payload().Get("count").Uint()
		}
	}
	if cut.CapacityDropped != 2 {
		t.Fatalf("retained trace loss: %+v", cut)
	}
	if protocols != 2 || lost != 2 {
		t.Fatalf("protocol facts=%d dropped=%d", protocols, lost)
	}
}

func TestShareDefaultDiagnosticsDoesNotRetainRetiredChannelGraphs(t *testing.T) {
	f := newShareFixture(t)
	o := newObservations(f.dependencies.Control, Request{})
	o.attachChannel(&wsrtc.Channel{})
	if len(o.completers) != 0 {
		t.Fatal("default share retained an unobserved WebRTC channel")
	}
	o.complete()
}

func TestShareLossCompletionAggregatesIndependentProducerQueues(t *testing.T) {
	f := newShareFixture(t)
	stream, _ := diagnostics.NewStream(32, nil)
	o := newObservations(f.dependencies.Control, Request{Diagnostics: stream.Sink()})
	o.loss(RelayObservations, 3)
	o.loss(RelayObservations, 4)
	o.loss(WebRTCObservations, ^uint64(0))
	o.loss(WebRTCObservations, 1)
	o.complete()
	counts := make(map[ObservationSource]uint64)
	stream.Close()
	for record := range stream.Records() {
		if record.Name() == "diagnostic_loss" {
			counts[ObservationSource(record.Payload().Get("source").Text())] = record.Payload().Get("count").Uint()
		}
	}
	if counts[RelayObservations] != 7 || counts[WebRTCObservations] != ^uint64(0) {
		t.Fatalf("loss cut=%v", counts)
	}
}

func testDiagnosticSink(t *testing.T) diagnostics.Sink {
	t.Helper()
	stream, err := diagnostics.NewStream(256, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream.Sink()
}
