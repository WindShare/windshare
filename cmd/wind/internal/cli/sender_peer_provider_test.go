package cli

import (
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/testrun"
)

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
	app.observeSenderPeerAttempt(peerFact(t, appobs.PeerLaneHelloAuthenticated, 0))
	if sink.event.Milestone != "" {
		t.Fatalf("pre-admission observation published milestone %q", sink.event.Milestone)
	}
	app.observeSenderPeerAttempt(peerFact(t, appobs.PeerAttemptAdmitted, appobs.LaneRouteTURN))
	if sink.event.Milestone != "" {
		t.Fatal("TURN admission was reported as a direct connection")
	}
	app.observeSenderPeerAttempt(peerFact(t, appobs.PeerAttemptAdmitted, appobs.LaneRouteDirect))
	if err := trace.close(); err != nil {
		t.Fatal(err)
	}
	if sink.event.Component != string(processTraceShareComponent) ||
		sink.event.Milestone != string(processTraceSenderDirectLane) ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) {
		t.Fatalf("sender admission event = %+v", sink.event)
	}
}
