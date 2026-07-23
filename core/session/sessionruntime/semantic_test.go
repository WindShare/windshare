package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverSemanticRegistryRejectsEverySignedMalformedSenderControl(t *testing.T) {
	validator, err := newReceiverSenderControlValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	operationID := id16[protocolsession.OperationID](1)
	base := protocolsession.ControlBinding{
		ShareInstance: id16Share(2), ProtocolSessionID: id16[protocolsession.ProtocolSessionID](3),
		LaneID: 4, LaneEpoch: 5, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(publicKey, base, validator)
	if err != nil {
		t.Fatal(err)
	}
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
	for index, kind := range kinds {
		t.Run(fmt.Sprintf("kind-%d", kind), func(t *testing.T) {
			sequence := uint64(index + 1)
			binding := base
			binding.Sequence = sequence
			binding.MessageKind = kind
			var messageOperation *protocolsession.OperationID
			domain := protocolsession.ControlDomainOperation
			switch kind {
			case protocolsession.MessageSessionTerminal:
				domain = protocolsession.ControlDomainSessionTerminal
			case protocolsession.MessageLaneAttach:
				domain = protocolsession.ControlDomainLaneAttach
				fallthrough
			default:
				messageOperation = &operationID
				binding.OperationID = operationID
				binding.HasOperationID = true
			}
			signed, err := protocolsession.SignControlBody(privateKey, domain, binding, []byte{0xf6})
			if err != nil {
				t.Fatal(err)
			}
			message, err := protocolsession.NewMessage(kind, messageOperation, signed)
			if err != nil {
				t.Fatal(err)
			}
			result, err := authenticator.AuthenticateInbound(sequence, message)
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

func TestMalformedSignedFinalTerminatesBeforeOperationTransition(t *testing.T) {
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil,
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
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	base := protocolsession.ControlBinding{
		ShareInstance: id16Share(7), ProtocolSessionID: id16[protocolsession.ProtocolSessionID](8),
		LaneID: 9, LaneEpoch: 10, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, _ := protocolsession.NewSenderControlAuthenticator(publicKey, base, validator)
	binding := base
	binding.Sequence = 1
	binding.MessageKind = protocolsession.MessageOpenResults
	binding.OperationID = operationID
	binding.HasOperationID = true
	signed, _ := protocolsession.SignControlBody(
		privateKey, protocolsession.ControlDomainOperation, binding, []byte{0xf6},
	)
	malformed, _ := protocolsession.NewMessage(protocolsession.MessageOpenResults, &operationID, signed)
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
	if !errors.Is(err, protocolsession.ErrInboundAuthentication) ||
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

func id16Share(value byte) [16]byte {
	return id16[[16]byte](value)
}
