package cli

import (
	"context"
	"testing"

	"github.com/windshare/windshare/connectivity/v2peer"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type testRelayObservationSource struct {
	endpoint    v2.RelayEndpoint
	stream      chan relayv2.LifecycleTrace
	completions int
	loss        uint64
}

func (source *testRelayObservationSource) Endpoint() v2.RelayEndpoint { return source.endpoint }
func (source *testRelayObservationSource) LifecycleTrace() <-chan relayv2.LifecycleTrace {
	return source.stream
}
func (source *testRelayObservationSource) CompleteObservations() relayv2.LifecycleObservationCompletion {
	source.completions++
	if source.completions == 1 {
		close(source.stream)
	}
	return relayv2.LifecycleObservationCompletion{Loss: relayv2.LifecycleObservationLoss{CapacityDropped: source.loss}}
}
func TestGetObservationKeepsConcurrentEndpointsAndDrainsReplacedEndpoint(t *testing.T) {
	runtime, _ := newGetReportingRuntime(t, false, true)
	observation := newGetObservation(runtime)
	first := &testRelayObservationSource{endpoint: v2.RelayEndpoint{Identity: v2.RelayIdentity{1}}, stream: make(chan relayv2.LifecycleTrace), loss: 2}
	second := &testRelayObservationSource{endpoint: v2.RelayEndpoint{Identity: v2.RelayIdentity{2}}, stream: make(chan relayv2.LifecycleTrace), loss: 3}
	replacement := &testRelayObservationSource{endpoint: first.endpoint, stream: make(chan relayv2.LifecycleTrace), loss: 4}
	observation.registerRelayConnection(first)
	observation.registerRelayConnection(second)
	if len(observation.state.relays) != 2 || first.completions != 0 || second.completions != 0 {
		t.Fatal("concurrent relay endpoint was cut")
	}
	observation.registerRelayConnection(replacement)
	if len(observation.state.relays) != 2 || first.completions != 1 || second.completions != 0 || replacement.completions != 0 {
		t.Fatal("endpoint replacement did not bound source ownership")
	}
	observation.complete(context.Background())
	observation.complete(context.Background())
	runtime.Close()
	if first.completions != 1 || second.completions != 1 || replacement.completions != 1 {
		t.Fatal("producer completion repeated or skipped")
	}
}

type testReceiverObservationSource struct {
	terminal    chan v2peer.ReceiverTerminationTrace
	diagnostic  chan v2peer.PeerDiagnosticObservation
	completions int
}

func (source *testReceiverObservationSource) ReceiverTerminationObservations() <-chan v2peer.ReceiverTerminationTrace {
	return source.terminal
}
func (source *testReceiverObservationSource) PeerDiagnostics() <-chan v2peer.PeerDiagnosticObservation {
	return source.diagnostic
}
func (source *testReceiverObservationSource) CompleteObservations() v2peer.ReceiverObservationCompletion {
	source.completions++
	if source.completions == 1 {
		close(source.terminal)
		close(source.diagnostic)
	}
	return v2peer.ReceiverObservationCompletion{}
}
func TestGetObservationDrainsPreviousReceiverGenerationBeforeDiscardingReader(t *testing.T) {
	runtime, _ := newGetReportingRuntime(t, false, true)
	observation := newGetObservation(runtime)
	previous := &testReceiverObservationSource{terminal: make(chan v2peer.ReceiverTerminationTrace), diagnostic: make(chan v2peer.PeerDiagnosticObservation)}
	next := &testReceiverObservationSource{terminal: make(chan v2peer.ReceiverTerminationTrace), diagnostic: make(chan v2peer.PeerDiagnosticObservation)}
	observation.registerReceiverFactory(previous, &receiverLocalStop{})
	observation.registerReceiverFactory(next, &receiverLocalStop{})
	if previous.completions != 1 || next.completions != 0 {
		t.Fatal("generation source ownership was overwritten")
	}
	observation.complete(context.Background())
	runtime.Close()
	if next.completions != 1 {
		t.Fatal("final generation not completed")
	}
}
