package sessioncore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestInboundRetiredPeerCandidateUsesOperationAuthorityWithoutResponseRoute(t *testing.T) {
	for _, terminal := range []protocolsession.MessageKind{
		protocolsession.MessageOperationError,
		protocolsession.MessageCancel,
	} {
		t.Run(map[protocolsession.MessageKind]string{
			protocolsession.MessageOperationError: "rejected",
			protocolsession.MessageCancel:         "cancelled",
		}[terminal], func(t *testing.T) {
			runtime, _ := newUnstartedRuntimeWithContinuations(
				t, protocolsession.RoleSender, protocolsession.OperationLimits{}, nil, continuationReplayClassifier{},
			)
			inbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
			id := id16[protocolsession.OperationID](171)
			request := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})
			if disposition, err := inbound.RouteInbound(context.Background(), request); err != nil || disposition != protocolsession.OperationDeliver {
				t.Fatalf("offer admission = %d, %v", disposition, err)
			}
			if _, err := runtime.router.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			route := runtime.routes.current(id)
			direction := protocolsession.DirectionSenderToReceiver
			if terminal == protocolsession.MessageCancel {
				direction = protocolsession.DirectionReceiverToSender
			}
			body := []byte{0xf6}
			if terminal == protocolsession.MessageOperationError {
				binding := runtime.senderControlBase(runtime.initial)
				binding.Sequence, binding.MessageKind = 1, terminal
				binding.OperationID, binding.HasOperationID = id, true
				var err error
				body, err = protocolsession.SignControlBody(
					ed25519.NewKeyFromSeed(bytes.Repeat([]byte{97}, ed25519.SeedSize)),
					protocolsession.ControlDomainOperation, binding, body,
				)
				if err != nil {
					t.Fatal(err)
				}
			}
			final := operationMessageForTest(t, terminal, id, body)
			if disposition, err := runtime.operations.Observe(direction, final); err != nil || disposition != protocolsession.OperationDeliver {
				t.Fatalf("operation retirement = %d, %v", disposition, err)
			}
			runtime.routes.releaseRoute(id, route)

			// A candidate admitted by the remote writer before the final can arrive
			// after the response route is gone, including on another physical lane.
			for _, lane := range []sessioncontract.LaneIdentity{runtime.initial, {ID: runtime.initial.ID + 1, Epoch: 1}} {
				candidate := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xf6})
				disposition, err := (laneInboundRouter{runtime: runtime, identity: lane}).RouteInbound(context.Background(), candidate)
				if err != nil || disposition != protocolsession.OperationDrop {
					t.Fatalf("late candidate on %v = %d, %v", lane, disposition, err)
				}
			}
			if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
				t.Fatal("late traffic revived a response route or operation")
			}

			// Removing a physical-route check must not bypass the retained
			// continuation contract's semantic validation.
			malformed := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xa0})
			if _, err := inbound.RouteInbound(context.Background(), malformed); !errors.Is(err, protocolsession.ErrInvalidMessage) {
				t.Fatalf("invalid late candidate = %v", err)
			}
			unknown := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id16[protocolsession.OperationID](172), []byte{0xf6})
			if _, err := inbound.RouteInbound(context.Background(), unknown); !errors.Is(err, protocolsession.ErrUnknownOperation) {
				t.Fatalf("unknown operation candidate = %v", err)
			}
		})
	}
}
