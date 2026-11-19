package receiversession

import (
	"context"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/framechannel"

	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestReceiverSemanticRegistryRejectsEveryAuthenticatedMalformedSenderControl(t *testing.T) {
	validator, err := newReceiverSenderControlValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	operationID := id16[protocolsession.OperationID](1)
	authenticator := protocolsession.SenderMessageValidator(validator)
	kinds := []protocolsession.MessageKind{
		protocolsession.MessageCatalogResult,
		protocolsession.MessageOpenResults,
		protocolsession.MessageOperationError,
		protocolsession.MessageSessionTerminal,
		protocolsession.MessageLaneAttach,
		protocolsession.MessageScanProgress,
		protocolsession.MessageOperationComplete,
		protocolsession.MessageLeaseResult,
		protocolsession.MessagePeerAnswer,
		protocolsession.MessagePeerCandidate,
	}
	for _, kind := range kinds {
		t.Run(fmt.Sprintf("kind-%d", kind), func(t *testing.T) {
			var messageOperation *protocolsession.OperationID
			if kind != protocolsession.MessageSessionTerminal {
				messageOperation = &operationID
			}
			message, err := protocolsession.NewMessage(kind, messageOperation, []byte{0xf6})
			if err != nil {
				t.Fatal(err)
			}
			result, err := authenticator.ValidateInbound(message)
			switch kind {
			case protocolsession.MessageOperationError,
				protocolsession.MessagePeerAnswer,
				protocolsession.MessagePeerCandidate:
				if err != nil || !result.HasOperationViolation() {
					t.Fatalf("signed malformed operation control lost structural violation: result=%+v err=%v", result, err)
				}
				return
			}
			if !errors.Is(err, protocolsession.ErrControlSemantic) || result.HasOperationViolation() {
				t.Fatalf("signed malformed control crossed authentication boundary: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestMalformedAuthenticatedFinalTerminatesBeforeOperationTransition(t *testing.T) {
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTracked: 4}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	router, err := protocolsession.NewRoleRouter(protocolsession.RoleReceiver, operations)
	if err != nil {
		t.Fatal(err)
	}
	operationID := id16[protocolsession.OperationID](6)
	requestBody, _ := protocolsession.EncodeBody([]any{})
	request, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, requestBody)
	if _, err := router.AdmitOutbound(request, protocolsession.OutboundOperationPermit{}); err != nil || operations.ActiveCount() != 1 {
		t.Fatalf("begin receiver operation: active=%d err=%v", operations.ActiveCount(), err)
	}

	validator, _ := newReceiverSenderControlValidator(nil)
	authenticator := protocolsession.SenderMessageValidator(validator)
	malformed, _ := protocolsession.NewMessage(protocolsession.MessageOpenResults, &operationID, []byte{0xf6})
	plaintext, _ := protocolsession.EncodeMessage(malformed)

	receiver, peer := newMemoryChannelPair()
	pump, err := protocolsession.NewProtocolPump(
		receiver,
		semanticTestOpener{opened: protocolsession.OpenedEnvelope{Sequence: 1, Plaintext: plaintext}},
		authenticator,
		router,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(context.Background(), framechannel.Frame{1}); err != nil {
		t.Fatal(err)
	}
	err = pump.Run(context.Background())
	if !errors.Is(err, protocolsession.ErrInboundSemantic) ||
		!errors.Is(err, protocolsession.ErrControlSemantic) {
		t.Fatalf("malformed final rejection = %v", err)
	}
	if !operations.Terminated() || operations.TombstoneCount() != 0 {
		t.Fatalf("malformed final changed operation state: terminal=%v tombstones=%d", operations.Terminated(), operations.TombstoneCount())
	}
}

func TestCatalogProgressStateIsMonotonicAndReplayIdempotent(t *testing.T) {
	firstAttempt := id16[catalog.ScanAttemptID](11)
	otherAttempt := id16[catalog.ScanAttemptID](12)
	var state catalogProgressState
	for _, progress := range []protocolsession.ScanProgress{
		{AttemptID: firstAttempt, DiscoveredEntries: 1},
		{AttemptID: firstAttempt, DiscoveredEntries: 256},
	} {
		if notify, err := state.observe(progress); err != nil || !notify {
			t.Fatalf("increasing progress = notify %v err %v", notify, err)
		}
	}
	if notify, err := state.observe(protocolsession.ScanProgress{
		AttemptID: firstAttempt, DiscoveredEntries: 256,
	}); err != nil || notify {
		t.Fatalf("equal replay = notify %v err %v", notify, err)
	}
	for name, progress := range map[string]protocolsession.ScanProgress{
		"regression":   {AttemptID: firstAttempt, DiscoveredEntries: 255},
		"substitution": {AttemptID: otherAttempt, DiscoveredEntries: 257},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := state.observe(progress); !errors.Is(err, ErrScanProgress) {
				t.Fatalf("hostile progress = %v", err)
			}
		})
	}
}

type semanticTestOpener struct {
	opened protocolsession.OpenedEnvelope
}

func (opener semanticTestOpener) Open([]byte) (protocolsession.OpenedEnvelope, error) {
	return opener.opened, nil
}
