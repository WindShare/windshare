package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
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
			id := id16[protocolsession.OperationID](171)
			request := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})
			if disposition, err := runtime.inbound.RouteInbound(runtime.initial, request); err != nil || disposition != protocolsession.OperationDeliver {
				t.Fatalf("offer admission = %d, %v", disposition, err)
			}
			if _, err := runtime.inbound.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			direction := protocolsession.DirectionSenderToReceiver
			if terminal == protocolsession.MessageCancel {
				direction = protocolsession.DirectionReceiverToSender
			}
			body := []byte{0xf6}
			final := operationMessageForTest(t, terminal, id, body)
			if admission, err := runtime.operations.ObserveInbound(direction, runtime.initial, final); err != nil || admission.disposition != protocolsession.OperationDeliver {
				t.Fatalf("operation retirement = %d, %v", admission.disposition, err)
			}

			// A candidate admitted by the remote writer before the final can arrive
			// after the operation is retired, including on another physical lane.
			for _, lane := range []session.LaneIdentity{runtime.initial, {ID: runtime.initial.ID + 1, Epoch: 1}} {
				candidate := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xf6})
				disposition, err := runtime.inbound.RouteInbound(lane, candidate)
				if err != nil || disposition != protocolsession.OperationDrop {
					t.Fatalf("late candidate on %v = %d, %v", lane, disposition, err)
				}
			}
			if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
				t.Fatal("late traffic revived a retired operation")
			}

			// Removing a physical-route check must not bypass the retained
			// continuation contract's semantic validation.
			malformed := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xa0})
			if _, err := runtime.inbound.RouteInbound(runtime.initial, malformed); !errors.Is(err, protocolsession.ErrInvalidMessage) {
				t.Fatalf("invalid late candidate = %v", err)
			}
			unknown := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id16[protocolsession.OperationID](172), []byte{0xf6})
			if _, err := runtime.inbound.RouteInbound(runtime.initial, unknown); !errors.Is(err, protocolsession.ErrUnknownOperation) {
				t.Fatalf("unknown operation candidate = %v", err)
			}
		})
	}
}
