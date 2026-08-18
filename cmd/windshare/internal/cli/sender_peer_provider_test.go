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
	const constructionCount = 100
	for construction := range constructionCount {
		app := &App{Stderr: io.Discard}
		factory, err := app.newSenderPeerFactory(nil, nil)
		if err != nil || factory == nil {
			t.Fatalf("construction %d production sender factory = %v, %v", construction, factory, err)
		}
		if factory.SenderAttemptObservations() != nil || factory.PeerDiagnostics() != nil {
			t.Fatalf("construction %d enabled a default observation stream", construction)
		}
		factory.CompleteObservations()
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
	app.observeSenderPeerAttempt(v2peer.SenderAttemptObservation{
		Stage: v2peer.SenderAttemptLaneHelloAuthenticated,
		Lane:  &sessionruntime.LaneIdentity{ID: 1, Epoch: 1},
	})
	if sink.event.Milestone != "" {
		t.Fatalf("pre-admission observation published milestone %q", sink.event.Milestone)
	}
	app.observeSenderPeerAttempt(v2peer.SenderAttemptObservation{
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
	config := senderPeerConfig(observations, false, nil)
	if config.SenderAttemptObservationCapacity != 0 || config.PeerDiagnosticObservationCapacity != 0 || config.DataChannels != nil {
		t.Fatalf("sender observer config = %#v", config)
	}
	processOnly := senderPeerConfig(observations, true, nil)
	if processOnly.SenderAttemptObservationCapacity == 0 || processOnly.PeerDiagnosticObservationCapacity != 0 || processOnly.DataChannels != nil {
		t.Fatalf("process-only observer config = %#v", processOnly)
	}
	if len(emitter.events) != 0 {
		t.Fatalf("default detailed events=%#v", emitter.events)
	}
}

func TestProcessTraceOnlySenderFactoryRetainsAndCompletesItsAttemptStream(t *testing.T) {
	observations := newShareObservations(&shareRecordingEmitter{})
	app := &App{processTrace: &processTrace{}}
	factory, err := app.newSenderPeerFactory(observations, nil)
	if err != nil {
		t.Fatal(err)
	}
	if factory.SenderAttemptObservations() == nil || factory.PeerDiagnostics() != nil {
		t.Fatalf("process-only streams: attempts=%v diagnostics=%v", factory.SenderAttemptObservations(), factory.PeerDiagnostics())
	}
	if observations.peerFactory != factory || observations.peerAttemptReader == nil {
		t.Fatal("process-only observation ownership was not retained")
	}
	observations.completeWithin()
	completion := factory.CompleteObservations()
	if completion.Attempts.Loss.CapacityDropped != 0 {
		t.Fatalf("completion = %+v", completion)
	}
}
