package cli

import (
	"io"
	"testing"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/internal/testrun"
)

func TestSenderPeerFactoryUsesProductionProviderForEveryZeroValueApp(t *testing.T) {
	for _, name := range []string{
		"WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE",
		"WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION",
		"WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256",
		"WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256",
	} {
		t.Setenv(name, "must-not-be-read")
	}
	app := &App{Stderr: io.Discard}
	factory, err := app.newSenderPeerFactory(nil, nil)
	if err != nil || factory == nil {
		t.Fatalf("production sender factory = %v, %v", factory, err)
	}
}

func TestSenderPeerAdmissionPublishesPrivateLaneMilestone(t *testing.T) {
	lookup := func(name string) (string, bool) {
		values := map[string]string{
			testrun.RunIDEnvironment:       "run-1",
			testrun.OperationIDEnvironment: "operation-1",
			testrun.ScenarioEnvironment:    "sender-peer-admission",
		}
		value, present := values[name]
		return value, present
	}
	sink := &recordingProcessTraceSink{}
	trace, err := newProcessTraceWithSink(
		lookup,
		func(testrun.Identity) (processTraceEventSink, error) { return sink, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{processTrace: trace}
	config := senderPeerConfig(
		nil,
		v2peer.SenderAttemptObserverFunc(app.observeSenderPeerAttempt),
		nil,
	)
	config.Observer.ObserveSenderAttempt(v2peer.SenderAttemptObservation{
		Stage: v2peer.SenderAttemptLaneAdmissionStarted,
		Lane:  &sessionruntime.LaneIdentity{ID: 1, Epoch: 1},
	})
	if sink.event.Milestone != "" {
		t.Fatalf("pre-admission observation published milestone %q", sink.event.Milestone)
	}
	config.Observer.ObserveSenderAttempt(v2peer.SenderAttemptObservation{
		Stage: v2peer.SenderAttemptAdmitted,
		Lane:  &sessionruntime.LaneIdentity{ID: 1, Epoch: 1},
	})
	if err := trace.close(); err != nil {
		t.Fatal(err)
	}
	if sink.event.Component != string(processTraceShareComponent) ||
		sink.event.Milestone != string(processTraceSenderDirectLane) ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) {
		t.Fatalf("sender admission event = %+v", sink.event)
	}
}

func TestSenderPeerConfigKeepsDetailedDiagnosticsDisabledByDefault(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	observations := newShareObservations(emitter)
	config := senderPeerConfig(observations, nil, nil)
	if config.Observer == nil || config.DataChannels == nil || config.DiagnosticObserver != nil {
		t.Fatalf("sender observer config = %#v", config)
	}
	adapter, ok := config.DataChannels.(senderDataChannelAdapter)
	if !ok || adapter.tracer != nil {
		t.Fatalf("data channel adapter = %#v", config.DataChannels)
	}
	router := config.Observer.(senderPeerObservationRouter)
	if router.command != nil || len(emitter.events) != 0 {
		t.Fatalf("default detailed observer = %#v, events=%#v", router.command, emitter.events)
	}
}
