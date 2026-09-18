package share

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/task"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestShareProtocolAndTerminalObservationAuthorityFollowsRequest(t *testing.T) {
	for _, test := range []struct {
		name    string
		request Request
	}{
		{"default", Request{}},
		{"diagnostics", Request{Diagnostics: true}},
		{"trace", Request{Diagnostics: true, TraceLifecycle: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newShareFixture()
			o := newObservations(f.dependencies.Control, test.request)
			config := o.runtimeConfig(f.relays, nil)
			if !config.ProtocolObservations.IsZero() != test.request.Diagnostics {
				t.Fatal("protocol producer ignored diagnostic authority")
			}
			if (config.TerminalSendObserver != nil) != test.request.TraceLifecycle || (config.SessionTerminalObserver != nil) != test.request.TraceLifecycle {
				t.Fatal("terminal observers ignored lifecycle trace authority")
			}
			o.complete()
		})
	}
}

func TestShareFinalObservationCutWaitsForRuntimeAndPreservesUpstreamLoss(t *testing.T) {
	f := newShareFixture()
	f.factory.entered, f.factory.release = make(chan struct{}), make(chan struct{})
	var producer observationstream.Producer[sessionruntime.ProtocolObservation]
	f.prepared.observeRuntime = func(config liveshare.RuntimeFactoryConfig) { producer = config.ProtocolObservations }
	f.factory.observeStop = func() {
		producer.TryPublish(sessionruntime.ProtocolOperationObservation{})
		producer.RecordDropped(2)
	}
	ctx, stop := context.WithCancelCause(context.Background())
	done := fixtureRun(f, ctx, Request{Diagnostics: true})
	if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.dependencies.Controller.Acknowledge(nil)
	if err := f.dependencies.Controller.Activated(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop(task.ErrShareStopped)
	<-f.factory.entered
	if !producer.TryPublish(sessionruntime.ProtocolOperationObservation{}) {
		t.Fatal("runtime observation authority was cut before join")
	}
	close(f.factory.release)
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil {
		t.Fatalf("result=%+v", result)
	}
	if producer.TryPublish(sessionruntime.ProtocolOperationObservation{}) {
		t.Fatal("completed share retained observation admission")
	}
	var protocols int
	var lost uint64
	for _, fact := range f.facts {
		if fact.Protocol != nil {
			protocols++
		}
		if fact.Loss != nil && fact.Loss.Source == ProtocolObservations {
			lost += fact.Loss.Dropped
		}
	}
	if len(result.Value.ObservationLosses) != 1 || result.Value.ObservationLosses[0].Source != ProtocolObservations || result.Value.ObservationLosses[0].Dropped != 2 {
		t.Fatalf("durable observation loss=%+v", result.Value.ObservationLosses)
	}
	if protocols != 2 || lost != 2 {
		t.Fatalf("protocol facts=%d dropped=%d", protocols, lost)
	}
}

func TestShareDefaultDiagnosticsDoesNotRetainRetiredChannelGraphs(t *testing.T) {
	f := newShareFixture()
	o := newObservations(f.dependencies.Control, Request{})
	o.attachChannel(&wsrtc.Channel{})
	if len(o.completers) != 0 {
		t.Fatal("default share retained an unobserved WebRTC channel")
	}
	o.complete()
}

func TestShareLossCompletionAggregatesIndependentProducerQueues(t *testing.T) {
	f := newShareFixture()
	o := newObservations(f.dependencies.Control, Request{})
	o.loss(RelayObservations, 3)
	o.loss(RelayObservations, 4)
	o.loss(WebRTCObservations, ^uint64(0))
	o.loss(WebRTCObservations, 1)
	o.complete()
	counts := make(map[ObservationSource]uint64)
	for _, fact := range f.facts {
		if fact.Loss != nil {
			counts[fact.Loss.Source] = fact.Loss.Dropped
		}
	}
	if counts[RelayObservations] != 7 || counts[WebRTCObservations] != ^uint64(0) {
		t.Fatalf("loss cut=%v", counts)
	}
}
