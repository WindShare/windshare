package v2peer

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
)

const peerTestSessionControlCapacity = 16

func (session *testPeerSession) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	session.sessionControlMu.Lock()
	defer session.sessionControlMu.Unlock()
	session.pathControlHandler = handler
}

func (session *testPeerSession) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	session.sessionControlMu.Lock()
	defer session.sessionControlMu.Unlock()
	session.turnHandler = handler
}

func (session *testPeerSession) SendPeerPathControl(ctx context.Context, body []byte) error {
	return session.sendSessionControl(ctx, protocolsession.MessagePeerPathControl, body)
}

func (session *testPeerSession) SendTURNControl(ctx context.Context, body []byte) error {
	return session.sendSessionControl(ctx, protocolsession.MessageTURNControl, body)
}

func (session *testPeerSession) sendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case session.sessionControls <- capturedControl{kind: kind, body: bytes.Clone(body)}:
		return nil
	}
}

func (session *testPeerSession) sessionControlHandler() func(context.Context, []byte) error {
	session.sessionControlMu.Lock()
	defer session.sessionControlMu.Unlock()
	return session.pathControlHandler
}

func TestSenderSessionControlsSurvivePeerSessionWrapper(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		factory, err := newTestSenderFactory(t, Config{})
		if err != nil {
			t.Fatal(err)
		}
		session := newTestPeerSession(1)
		// Embedding only the public contract reproduces an ordinary decorator.
		wrapped := struct{ sendersession.PeerSession }{PeerSession: session}
		_, ctx, cancel, done := startSenderTestRuntime(t, factory, wrapped)
		stop := sync.OnceFunc(func() { stopSenderTestRuntime(t, cancel, done) })
		t.Cleanup(stop)
		assertPeerState(t, func() bool {
			return session.sessionControlHandler() != nil
		})
		pathHandler := session.sessionControlHandler()
		path := protocolsession.PeerPathControl{
			PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2},
			ControlSequence: 1, Kind: protocolsession.PeerPathDemand,
			ValidFor: nativepeer.ControlLifetime,
		}
		applyPath := func() {
			t.Helper()
			body, encodeErr := protocolsession.EncodePeerPathControl(path)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if handleErr := pathHandler(ctx, body); handleErr != nil {
				t.Fatal(handleErr)
			}
		}
		applyPath()
		path.Kind, path.ControlSequence, path.ValidFor = protocolsession.PeerPathRevoke, 2, 0
		applyPath()
		if !factory.native.Retired([16]byte(session.sessionID), v2signal.PeerPathID(path.PeerPathID)) {
			t.Fatal("wrapped path controls did not retire the native path")
		}

		stop()
		if session.sessionControlHandler() != nil {
			t.Fatal("sender shutdown retained session control handler")
		}
		select {
		case control := <-session.sessionControls:
			t.Fatalf("unconfigured session sent a control: %+v", control)
		default:
		}
	})
}
