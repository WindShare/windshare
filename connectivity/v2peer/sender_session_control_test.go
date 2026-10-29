package v2peer

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	"github.com/windshare/windshare/core/session/turncontrol"
)

const (
	peerTestSessionControlCapacity = 16
	peerTestTURNProfile            = "session-control-test"
)

func (session *testPeerSession) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	session.sessionControlMu.Lock()
	defer session.sessionControlMu.Unlock()
	session.pathControlHandler = handler
}

func (session *testPeerSession) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	session.sessionControlMu.Lock()
	defer session.sessionControlMu.Unlock()
	session.turnControlHandler = handler
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

func (session *testPeerSession) sessionControlHandlers() (func(context.Context, []byte) error, func(context.Context, []byte) error) {
	session.sessionControlMu.Lock()
	defer session.sessionControlMu.Unlock()
	return session.pathControlHandler, session.turnControlHandler
}

type sessionControlAccounts struct {
	requests chan turnpath.Subject
}

func (*sessionControlAccounts) Inventory() turnpath.Inventory {
	return turnpath.Inventory{
		Revision: 1,
		Profiles: []turnpath.Profile{{
			Identity: iceconfig.Identity{Provider: "test", Account: "test", Profile: peerTestTURNProfile},
			Revision: 1,
		}},
	}
}

func (accounts *sessionControlAccounts) Acquire(ctx context.Context, _ string, subject turnpath.Subject) (*turnpath.Lease, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case accounts.requests <- subject:
		return nil, turnpath.ErrBudget
	}
}

func TestSenderSessionControlsSurvivePeerSessionWrapper(t *testing.T) {
	for _, withTURN := range []bool{false, true} {
		name := "without TURN service"
		if withTURN {
			name = "with TURN service"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {

				accounts := &sessionControlAccounts{requests: make(chan turnpath.Subject, 1)}
				config := Config{}
				if withTURN {
					config.TURN = accounts
				}
				factory, err := newTestSenderFactory(t, config)
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
					path, turn := session.sessionControlHandlers()
					return path != nil && turn != nil
				})
				pathHandler, turnHandler := session.sessionControlHandlers()
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

				if withTURN {
					receiveTURN := func() turncontrol.Message {
						t.Helper()
						control := receiveTest(t, session.sessionControls)
						if control.kind != protocolsession.MessageTURNControl {
							t.Fatalf("TURN response used control family %v", control.kind)
						}
						message, decodeErr := turncontrol.Decode(control.body)
						if decodeErr != nil {
							t.Fatal(decodeErr)
						}
						if message.Header().SessionID != [16]byte(session.sessionID) {
							t.Fatal("TURN response changed the session identity")
						}
						return message
					}
					message := receiveTURN()
					available, ok := message.(turncontrol.Available)
					if !ok || len(available.Profiles) != 1 || available.Profiles[0] != peerTestTURNProfile {
						t.Fatalf("TURN inventory = %#v", message)
					}
					select {
					case <-accounts.requests:
						t.Fatal("registering TURN controls acquired credentials before demand")
					default:
					}
					demand, encodeErr := turncontrol.Encode(turncontrol.Demand{
						Authority: available.Authority, HoldFor: time.Minute,
					})
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					if handleErr := turnHandler(ctx, demand); handleErr != nil {
						t.Fatal(handleErr)
					}
					request := receiveTest(t, accounts.requests)
					if request.SessionID != [16]byte(session.sessionID) || request.PathID == ([16]byte{}) {
						t.Fatalf("TURN acquisition lost its session/path authority: %+v", request)
					}
					message = receiveTURN()
					if unavailable, ok := message.(turncontrol.Unavailable); !ok || unavailable.Profile != peerTestTURNProfile {
						t.Fatalf("TURN demand response = %#v", message)
					}
				}

				stop()
				if path, turn := session.sessionControlHandlers(); path != nil || turn != nil {
					t.Fatal("sender shutdown retained session control handlers")
				}
				if !withTURN {
					select {
					case control := <-session.sessionControls:
						t.Fatalf("unconfigured session sent a control: %+v", control)
					default:
					}
				}

			})
		})
	}
}
