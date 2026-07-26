package protocolsession

import (
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestPrepareSenderControlRejectsInvalidAuthorityBeforeOwningInputs(t *testing.T) {
	key := vectorSenderSigningKey()
	base := testControlBase(t)
	operationID := testOperationID(231)
	semantic := mustControlBody(t, map[uint64]any{0: uint64(1)})

	tests := []struct {
		name        string
		base        ControlBinding
		kind        MessageKind
		operationID *OperationID
		semantic    []byte
		want        error
	}{
		{
			name: "invalid fixed binding", base: bindingWith(base, func(value *ControlBinding) { value.LaneID = 0 }),
			kind: MessageOperationError, operationID: &operationID, semantic: semantic, want: ErrControlBinding,
		},
		{
			name: "receiver-authored kind", base: base, kind: MessageCancel,
			operationID: &operationID, semantic: semantic, want: ErrControlBinding,
		},
		{
			name: "missing operation identity", base: base, kind: MessageOperationError,
			semantic: semantic, want: ErrInvalidOperationID,
		},
		{
			name: "non-canonical semantic body", base: base, kind: MessageOperationError,
			operationID: &operationID, semantic: nil, want: ErrControlBody,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PrepareSenderControl(
				key, test.base, test.kind, test.operationID, test.semantic,
			); !errors.Is(err, test.want) {
				t.Fatalf("PrepareSenderControl error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPrepareSenderControlRejectsEachSerializationBoundary(t *testing.T) {
	key := vectorSenderSigningKey()
	base := testControlBase(t)
	operationID := testOperationID(232)
	signature := make([]byte, ed25519.SignatureSize)

	// The unsigned and signed wrappers have distinct size ceilings in practice:
	// both use the envelope limit, but the signature consumes part of that limit.
	// Exercising the exact cut prevents a future encoder change from moving an
	// oversized control past preparation and into the writer-owned sequence path.
	unsignedLimit := largestControlSemanticAcceptedBy(t, func(semantic []byte) error {
		_, err := encodeUnsignedControlWrapper(semantic)
		return err
	})
	if _, err := encodeSignedControlWrapper(unsignedLimit, signature); !errors.Is(err, ErrControlBody) {
		t.Fatalf("signed wrapper at unsigned limit error = %v, want %v", err, ErrControlBody)
	}
	if _, err := PrepareSenderControl(
		key, base, MessageOperationError, &operationID, unsignedLimit,
	); !errors.Is(err, ErrControlBody) {
		t.Fatalf("prepare at unsigned wrapper limit error = %v, want %v", err, ErrControlBody)
	}

	signedLimit := largestControlSemanticAcceptedBy(t, func(semantic []byte) error {
		_, err := encodeSignedControlWrapper(semantic, signature)
		return err
	})
	if _, err := PrepareSenderControl(
		key, base, MessageOperationError, &operationID, signedLimit,
	); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("prepare at signed wrapper limit error = %v, want %v", err, ErrMessageTooLarge)
	}

	oversizedSemantic, err := EncodeBody(make([]byte, MaxEnvelopePlaintextBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encodeUnsignedControlWrapper(oversizedSemantic); !errors.Is(err, ErrControlBody) {
		t.Fatalf("oversized unsigned wrapper error = %v, want %v", err, ErrControlBody)
	}
}

func TestSenderControlAuthenticatorClassifiesAuthenticatedSemanticFailures(t *testing.T) {
	key := vectorSenderSigningKey()
	base := testControlBase(t)
	operationID := testOperationID(233)
	semantic := mustControlBody(t, map[uint64]any{0: uint64(1)})
	authenticator, err := NewSenderControlAuthenticator(
		key.Public().(ed25519.PublicKey), base, nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		kind          MessageKind
		wantViolation AuthenticatedOperationViolationCode
	}{
		{
			name: "malformed operation failure", kind: MessageOperationError,
			wantViolation: AuthenticatedOperationViolationMalformedFailure,
		},
		{
			name: "malformed peer control", kind: MessagePeerCandidate,
			wantViolation: AuthenticatedOperationViolationMalformedPeerControl,
		},
		{name: "unowned external semantic", kind: MessageCatalogResult},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sequence := uint64(index + 1)
			binding := base
			binding.Sequence = sequence
			binding.MessageKind = test.kind
			binding.OperationID = operationID
			binding.HasOperationID = true
			signed, err := SignControlBody(key, ControlDomainOperation, binding, semantic)
			if err != nil {
				t.Fatal(err)
			}
			message, err := NewMessage(test.kind, &operationID, signed)
			if err != nil {
				t.Fatal(err)
			}

			result, err := authenticator.AuthenticateInbound(sequence, message)
			if test.wantViolation == 0 {
				if !errors.Is(err, ErrControlSemantic) || result.HasOperationViolation() {
					t.Fatalf("authentication = %+v, %v, want semantic rejection", result, err)
				}
				return
			}
			if err != nil || !result.HasOperationViolation() || result.operationViolation.Code() != test.wantViolation {
				t.Fatalf("authentication = %+v, %v, want violation %d", result, err, test.wantViolation)
			}
			if err := authenticator.Verify(sequence, message); !errors.Is(err, ErrControlSemantic) {
				t.Fatalf("Verify error = %v, want %v", err, ErrControlSemantic)
			}
		})
	}
}

func TestControlBindingAndWrapperDefensesRejectMalformedInputs(t *testing.T) {
	base := testControlBase(t)
	operationID := testOperationID(234)
	semantic := mustControlBody(t, map[uint64]any{0: uint64(1)})
	operationBinding := base
	operationBinding.Sequence = 1
	operationBinding.MessageKind = MessageOperationError
	operationBinding.OperationID = operationID
	operationBinding.HasOperationID = true

	if _, err := ControlSignaturePreimage(ControlDomainOperation, operationBinding, nil); !errors.Is(err, ErrControlBody) {
		t.Fatalf("preimage with empty semantic error = %v, want %v", err, ErrControlBody)
	}
	if _, err := SignControlBody(
		vectorSenderSigningKey(), ControlDomainOperation,
		bindingWith(operationBinding, func(value *ControlBinding) { value.Direction = DirectionReceiverToSender }),
		semantic,
	); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("sign with invalid binding error = %v, want %v", err, ErrControlBinding)
	}
	if _, err := ControlSignaturePreimage(
		ControlDomainOperation,
		bindingWith(operationBinding, func(value *ControlBinding) { value.OperationID = OperationID{} }),
		semantic,
	); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("zero bound operation identity error = %v, want %v", err, ErrControlBinding)
	}
	terminalBinding := base
	terminalBinding.Sequence = 1
	terminalBinding.MessageKind = MessageCatalogResult
	if _, err := ControlSignaturePreimage(
		ControlDomainSessionTerminal, terminalBinding, semantic,
	); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("wrong terminal kind error = %v, want %v", err, ErrControlBinding)
	}
	if _, err := ControlDomain(255).value(); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("unknown domain value error = %v, want %v", err, ErrControlBinding)
	}

	if _, err := encodeSignedControlWrapper(semantic, make([]byte, ed25519.SignatureSize-1)); !errors.Is(err, ErrControlSignature) {
		t.Fatalf("short wrapper signature error = %v, want %v", err, ErrControlSignature)
	}
	if _, err := encodeSignedControlWrapper(nil, make([]byte, ed25519.SignatureSize)); !errors.Is(err, ErrControlBody) {
		t.Fatalf("empty signed semantic error = %v, want %v", err, ErrControlBody)
	}
	for name, encoded := range map[string][]byte{
		"empty":    nil,
		"oversize": make([]byte, MaxEnvelopePlaintextBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeSignedControlBody(encoded); !errors.Is(err, ErrControlBody) {
				t.Fatalf("decode error = %v, want %v", err, ErrControlBody)
			}
		})
	}
	badSignature := mustControlBody(t, map[uint64]any{
		controlWrapperVersionKey:  uint64(controlWrapperVersion),
		controlWrapperSemanticKey: map[uint64]any{0: uint64(1)},
		controlSignatureKey:       []byte{1},
	})
	if _, _, err := decodeSignedControlBody(badSignature); !errors.Is(err, ErrControlSignature) {
		t.Fatalf("decode short signature error = %v, want %v", err, ErrControlSignature)
	}
}

func TestControlSemanticDecodersRejectCanonicalSchemaViolations(t *testing.T) {
	attempt := catalog.ScanAttemptID{1}
	if _, err := EncodeScanProgress(ScanProgress{}); !errors.Is(err, ErrInvalidScanProgress) {
		t.Fatalf("zero scan attempt error = %v, want %v", err, ErrInvalidScanProgress)
	}
	for name, encoded := range map[string][]byte{
		"non-canonical": {0xa1, 0x00, 0x18, 0x01},
		"wrong field type": mustControlBody(t, map[uint64]any{
			0: "1", 1: attempt.Bytes(), 2: uint64(1),
		}),
		"invalid attempt": mustControlBody(t, map[uint64]any{
			0: uint64(1), 1: []byte{1}, 2: uint64(1),
		}),
	} {
		t.Run("scan "+name, func(t *testing.T) {
			if _, err := DecodeScanProgress(encoded); !errors.Is(err, ErrInvalidScanProgress) {
				t.Fatalf("DecodeScanProgress error = %v, want %v", err, ErrInvalidScanProgress)
			}
		})
	}

	if _, err := EncodeSessionTerminal(SessionTerminal{}); !errors.Is(err, ErrInvalidSessionTerminal) {
		t.Fatalf("empty terminal error = %v, want %v", err, ErrInvalidSessionTerminal)
	}
	for name, encoded := range map[string][]byte{
		"non-canonical": {0xa1, 0x00, 0x18, 0x01},
		"wrong field type": mustControlBody(t, map[uint64]any{
			0: uint64(1), 1: "4097", 2: "stopped",
		}),
		"semantic rejection": mustControlBody(t, map[uint64]any{
			0: uint64(1), 1: uint64(SessionTerminalCodeFirst), 2: "",
		}),
	} {
		t.Run("terminal "+name, func(t *testing.T) {
			if _, err := DecodeSessionTerminal(encoded); !errors.Is(err, ErrInvalidSessionTerminal) {
				t.Fatalf("DecodeSessionTerminal error = %v, want %v", err, ErrInvalidSessionTerminal)
			}
		})
	}
}

func testControlBase(t *testing.T) ControlBinding {
	t.Helper()
	_, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	return ControlBinding{
		ShareInstance:     envelopeBinding.ShareInstance,
		ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID:            1,
		LaneEpoch:         1,
		Direction:         DirectionSenderToReceiver,
	}
}

func largestControlSemanticAcceptedBy(t *testing.T, accept func([]byte) error) []byte {
	t.Helper()
	low, high := 0, MaxEnvelopePlaintextBytes
	var largest []byte
	for low <= high {
		middle := low + (high-low)/2
		semantic, err := EncodeBody(make([]byte, middle))
		if err != nil {
			t.Fatal(err)
		}
		if err := accept(semantic); err == nil {
			largest = semantic
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if largest == nil {
		t.Fatal("no canonical byte-string semantic body fit the control wrapper")
	}
	return largest
}
