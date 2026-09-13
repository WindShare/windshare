package v2endpoint

import (
	"fmt"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

func TestResumePublishesSenderWindowBeforeConcurrentReceiverJoin(t *testing.T) {
	var server *Server
	observed := make(chan error, 1)
	tracer := v2route.ResumeTraceFunc(func(event v2route.ResumeTrace) {
		if event.Phase != v2route.ResumeCommitPhase || event.Outcome != "accepted" {
			return
		}
		// Commit tracing runs after the registry lock is released but before
		// serveResume returns, forcing the earliest possible receiver admission.
		receiver := newEndpointTestConnection("immediate-resume-receiver", nil, func() {})
		if !server.connections.add(receiver) {
			observed <- fmt.Errorf("receiver connection was not admitted")
			return
		}
		defer server.connections.complete(receiver.ref)
		joined, err := server.registry.Join(event.ShareID, receiver.ref)
		if err != nil || joined.Status != v2route.JoinReady {
			observed <- fmt.Errorf("immediate join = %+v, %v", joined, err)
			return
		}
		if !server.activateReceiverSession(receiver, event.ShareID, joined) {
			observed <- fmt.Errorf("immediate receiver session was not activated")
			return
		}
		sender, _, _ := server.connections.resolve(joined.Sender)
		sender.sessionMu.Lock()
		window := sender.windows[joined.RelaySessionID]
		ready := window != nil && window.frames == v2.SenderWindowFrames && window.bytes == v2.SenderWindowBytes
		sender.sessionMu.Unlock()
		server.cleanup(receiver)
		if !ready {
			observed <- fmt.Errorf("sender window was not installed before route became joinable")
			return
		}
		observed <- nil
	})
	var fixture endpointFixture
	server, fixture = newResumeTestServerWithTracer(t, tracer)
	dialResumeTestSender(t, server, fixture, false)
	dialResumeTestSender(t, server, fixture, true)
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	if _, err := server.registry.Join(fixture.init.ShareID, endpointTestConnectionRef("after-immediate-join")); err != nil {
		t.Fatalf("immediate receiver cleanup damaged resumed route: %v", err)
	}
}
