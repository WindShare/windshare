package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"strconv"
	"testing"
)

type laneControlVector struct {
	Name                 string `json:"name"`
	GrantLaneEpoch       uint32 `json:"grantLaneEpoch"`
	GrantLaneID          uint32 `json:"grantLaneId"`
	GrantOperationIDB64  string `json:"grantOperationIdB64"`
	GrantPlaintextB64    string `json:"grantPlaintextB64"`
	GrantPreimageB64     string `json:"grantPreimageB64"`
	GrantSequence        string `json:"grantSequence"`
	GrantSignatureB64    string `json:"grantSignatureB64"`
	ProtocolSessionIDB64 string `json:"protocolSessionIdB64"`
	ShareInstanceB64     string `json:"shareInstanceB64"`
	GrantSemanticBodyB64 string `json:"grantSemanticBodyCborB64"`
	GrantSignedBodyB64   string `json:"grantSignedBodyB64"`
	UnsignedGrantCBORB64 string `json:"unsignedGrantCborB64"`
}

func TestControlSignaturesMatchAllGoldenDomains(t *testing.T) {
	operation, _, operationEnvelope := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, err := OperationIDFromBytes(decodeB64(t, operation.OperationIDB64))
	if err != nil {
		t.Fatalf("decode operation identity: %v", err)
	}
	assertGoldenControl(t, goldenControlCase{
		domain: ControlDomainOperation,
		binding: ControlBinding{
			ShareInstance: operationEnvelope.ShareInstance, ProtocolSessionID: operationEnvelope.ProtocolSessionID,
			LaneID: operation.LaneID, LaneEpoch: operation.LaneEpoch, Direction: DirectionSenderToReceiver,
			Sequence: parseSequence(t, operation.Sequence), MessageKind: MessageOperationError,
			OperationID: operationID, HasOperationID: true,
		},
		semanticBody: decodeB64(t, operation.SemanticBodyB64), unsignedWrapper: decodeB64(t, operation.UnsignedControlB64),
		preimage: decodeB64(t, operation.ControlPreimageB64), signature: decodeB64(t, operation.ControlSignatureB64),
		signedBody: decodeB64(t, operation.SignedControlB64), plaintext: decodeB64(t, operation.PlaintextB64),
	})

	terminal, _, terminalEnvelope := loadEnvelopeVector(t, "sender-signed-session-terminal")
	assertGoldenControl(t, goldenControlCase{
		domain: ControlDomainSessionTerminal,
		binding: ControlBinding{
			ShareInstance: terminalEnvelope.ShareInstance, ProtocolSessionID: terminalEnvelope.ProtocolSessionID,
			LaneID: terminal.LaneID, LaneEpoch: terminal.LaneEpoch, Direction: DirectionSenderToReceiver,
			Sequence: parseSequence(t, terminal.Sequence), MessageKind: MessageSessionTerminal,
		},
		semanticBody: decodeB64(t, terminal.SemanticBodyB64), unsignedWrapper: decodeB64(t, terminal.UnsignedControlB64),
		preimage: decodeB64(t, terminal.ControlPreimageB64), signature: decodeB64(t, terminal.ControlSignatureB64),
		signedBody: decodeB64(t, terminal.SignedControlB64), plaintext: decodeB64(t, terminal.PlaintextB64),
	})

	var lane laneControlVector
	decodeSessionCase(t, "sender-granted-lane-attach", &lane)
	share := mustShare(t, decodeB64(t, lane.ShareInstanceB64))
	sessionID, err := ProtocolSessionIDFromBytes(decodeB64(t, lane.ProtocolSessionIDB64))
	if err != nil {
		t.Fatalf("decode lane protocol session identity: %v", err)
	}
	laneOperationID, err := OperationIDFromBytes(decodeB64(t, lane.GrantOperationIDB64))
	if err != nil {
		t.Fatalf("decode lane operation identity: %v", err)
	}
	assertGoldenControl(t, goldenControlCase{
		domain: ControlDomainLaneAttach,
		binding: ControlBinding{
			ShareInstance: share, ProtocolSessionID: sessionID,
			LaneID: lane.GrantLaneID, LaneEpoch: lane.GrantLaneEpoch, Direction: DirectionSenderToReceiver,
			Sequence: parseSequence(t, lane.GrantSequence), MessageKind: MessageLaneAttach,
			OperationID: laneOperationID, HasOperationID: true,
		},
		semanticBody: decodeB64(t, lane.GrantSemanticBodyB64), unsignedWrapper: decodeB64(t, lane.UnsignedGrantCBORB64),
		preimage: decodeB64(t, lane.GrantPreimageB64), signature: decodeB64(t, lane.GrantSignatureB64),
		signedBody: decodeB64(t, lane.GrantSignedBodyB64), plaintext: decodeB64(t, lane.GrantPlaintextB64),
	})
}

type goldenControlCase struct {
	domain          ControlDomain
	binding         ControlBinding
	semanticBody    []byte
	unsignedWrapper []byte
	preimage        []byte
	signature       []byte
	signedBody      []byte
	plaintext       []byte
}

func assertGoldenControl(t *testing.T, vector goldenControlCase) {
	t.Helper()
	preimage, err := ControlSignaturePreimage(vector.domain, vector.binding, vector.semanticBody)
	if err != nil {
		t.Fatalf("build control preimage: %v", err)
	}
	assertBytes(t, "control signature preimage", preimage, vector.preimage)
	signingKey := vectorSenderSigningKey()
	assertBytes(t, "control signature", ed25519.Sign(signingKey, preimage), vector.signature)

	unsignedWrapper, err := encodeUnsignedControlWrapper(vector.semanticBody)
	if err != nil {
		t.Fatalf("encode unsigned control wrapper: %v", err)
	}
	assertBytes(t, "unsigned control wrapper", unsignedWrapper, vector.unsignedWrapper)
	signedBody, err := SignControlBody(signingKey, vector.domain, vector.binding, vector.semanticBody)
	if err != nil {
		t.Fatalf("sign control body: %v", err)
	}
	assertBytes(t, "signed control wrapper", signedBody, vector.signedBody)
	message, err := DecodeMessage(vector.plaintext)
	if err != nil {
		t.Fatalf("decode vector control plaintext: %v", err)
	}
	assertBytes(t, "signed control body", signedBody, message.Body())
	verified, err := VerifyControlBody(signingKey.Public().(ed25519.PublicKey), vector.domain, vector.binding, signedBody)
	if err != nil {
		t.Fatalf("verify control body: %v", err)
	}
	assertBytes(t, "verified semantic body", verified, vector.semanticBody)
}

func TestControlSignatureRejectsEveryDeliveryAxisMutation(t *testing.T) {
	vector, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, err := OperationIDFromBytes(decodeB64(t, vector.OperationIDB64))
	if err != nil {
		t.Fatalf("decode operation identity: %v", err)
	}
	binding := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: vector.LaneID, LaneEpoch: vector.LaneEpoch, Direction: DirectionSenderToReceiver,
		Sequence: parseSequence(t, vector.Sequence), MessageKind: MessageOperationError,
		OperationID: operationID, HasOperationID: true,
	}
	unsigned := decodeB64(t, vector.UnsignedControlB64)
	signingKey := vectorSenderSigningKey()
	signed, err := SignControlBody(signingKey, ControlDomainOperation, binding, unsigned)
	if err != nil {
		t.Fatalf("sign control body: %v", err)
	}
	publicKey := signingKey.Public().(ed25519.PublicKey)

	validMutations := []struct {
		name   string
		mutate func(*ControlBinding)
	}{
		{"share", func(value *ControlBinding) { value.ShareInstance[0] ^= 1 }},
		{"protocol session", func(value *ControlBinding) { value.ProtocolSessionID[0] ^= 1 }},
		{"lane", func(value *ControlBinding) { value.LaneID++ }},
		{"epoch", func(value *ControlBinding) { value.LaneEpoch++ }},
		{"sequence", func(value *ControlBinding) { value.Sequence++ }},
		{"operation identity", func(value *ControlBinding) { value.OperationID[0] ^= 1 }},
	}
	for _, mutation := range validMutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := binding
			mutation.mutate(&changed)
			if _, err := VerifyControlBody(publicKey, ControlDomainOperation, changed, signed); !errors.Is(err, ErrControlSignature) {
				t.Fatalf("got %v, want ErrControlSignature", err)
			}
		})
	}

	otherUnsigned := mustControlBody(t, map[uint64]any{0: uint64(1), 1: uint64(4)})
	if _, err := VerifyControlBody(publicKey, ControlDomainOperation, binding, mustSignedControlBody(t, signed, otherUnsigned)); !errors.Is(err, ErrControlSignature) {
		t.Fatalf("body substitution: got %v", err)
	}
	wrongKey := ed25519.NewKeyFromSeed(sequentialBytes(0x21, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, err := VerifyControlBody(wrongKey, ControlDomainOperation, binding, signed); !errors.Is(err, ErrControlSignature) {
		t.Fatalf("wrong sender key: got %v", err)
	}
	if _, err := VerifyControlBody(publicKey, ControlDomainLaneAttach, binding, signed); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("domain substitution: got %v", err)
	}
}

func TestSenderControlWrapperPreservesArraySemanticsAndRejectsShapeDrift(t *testing.T) {
	vector, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, err := OperationIDFromBytes(decodeB64(t, vector.OperationIDB64))
	if err != nil {
		t.Fatal(err)
	}
	binding := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: vector.LaneID, LaneEpoch: vector.LaneEpoch, Direction: DirectionSenderToReceiver,
		Sequence: 9, MessageKind: MessagePeerAnswer, OperationID: operationID, HasOperationID: true,
	}
	semanticBody, err := EncodeBody([]any{uint64(1), []byte{1, 2, 3}, "answer"})
	if err != nil {
		t.Fatal(err)
	}
	key := vectorSenderSigningKey()
	signed, err := SignControlBody(key, ControlDomainOperation, binding, semanticBody)
	if err != nil {
		t.Fatalf("sign array semantic body: %v", err)
	}
	verified, err := VerifyControlBody(
		key.Public().(ed25519.PublicKey), ControlDomainOperation, binding, signed,
	)
	if err != nil || !bytes.Equal(verified, semanticBody) {
		t.Fatalf("verified semantic body changed: %x, %v", verified, err)
	}

	_, signature, err := decodeSignedControlBody(signed)
	if err != nil {
		t.Fatal(err)
	}
	var semanticValue any
	if err := messageDecMode.Unmarshal(semanticBody, &semanticValue); err != nil {
		t.Fatal(err)
	}
	malformedWrappers := [][]byte{
		mustControlBody(t, map[uint64]any{0: uint64(2), 1: semanticValue, 255: signature}),
		mustControlBody(t, map[uint64]any{0: uint64(1), 255: signature}),
		mustControlBody(t, map[uint64]any{0: uint64(1), 1: semanticValue, 2: nil, 255: signature}),
	}
	for index, malformed := range malformedWrappers {
		if _, err := VerifyControlBody(
			key.Public().(ed25519.PublicKey), ControlDomainOperation, binding, malformed,
		); !errors.Is(err, ErrControlBody) {
			t.Fatalf("malformed wrapper %d error = %v", index, err)
		}
	}
}

func TestControlBodyAndBindingValidationFailClosed(t *testing.T) {
	vector, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, err := OperationIDFromBytes(decodeB64(t, vector.OperationIDB64))
	if err != nil {
		t.Fatalf("decode operation identity: %v", err)
	}
	binding := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: vector.LaneID, Direction: DirectionSenderToReceiver, MessageKind: MessageOperationError,
		OperationID: operationID, HasOperationID: true,
	}
	unsigned := decodeB64(t, vector.UnsignedControlB64)
	key := vectorSenderSigningKey()

	malformedBodies := [][]byte{
		nil,
		{0xa1, 0x00, 0x18, 0x01},
		{0xa1, 0x00, 0xf9, 0x3e, 0x00},
		{0xa1, 0x00, 0xf7},
		{0xa1, 0x00, 0x63, 0x65, 0xcc, 0x81},
	}
	for index, body := range malformedBodies {
		if _, err := SignControlBody(key, ControlDomainOperation, binding, body); !errors.Is(err, ErrControlBody) {
			t.Fatalf("malformed body %d: got %v", index, err)
		}
	}
	if _, err := SignControlBody(key[:ed25519.PrivateKeySize-1], ControlDomainOperation, binding, unsigned); !errors.Is(err, ErrControlSigningKey) {
		t.Fatalf("short private key: got %v", err)
	}
	if _, err := VerifyControlBody(key.Public().(ed25519.PublicKey)[:ed25519.PublicKeySize-1], ControlDomainOperation, binding, unsigned); !errors.Is(err, ErrControlSigningKey) {
		t.Fatalf("short public key: got %v", err)
	}
	if _, err := VerifyControlBody(key.Public().(ed25519.PublicKey), ControlDomainOperation, binding, unsigned); !errors.Is(err, ErrControlBody) {
		t.Fatalf("missing signature: got %v", err)
	}

	invalidBindings := []ControlBinding{
		{},
		bindingWith(binding, func(value *ControlBinding) { value.Direction = DirectionReceiverToSender }),
		bindingWith(binding, func(value *ControlBinding) { value.HasOperationID = false }),
		bindingWith(binding, func(value *ControlBinding) { value.MessageKind = MessageListChildren }),
	}
	for index, invalid := range invalidBindings {
		if _, err := ControlSignaturePreimage(ControlDomainOperation, invalid, unsigned); !errors.Is(err, ErrControlBinding) {
			t.Fatalf("invalid binding %d: got %v", index, err)
		}
	}
	if _, err := ControlSignaturePreimage(ControlDomain(255), binding, unsigned); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("unknown control domain: got %v", err)
	}
}

func TestSenderControlAuthenticatorRequiresSenderAuthorityBeforeDispatch(t *testing.T) {
	vector, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, err := OperationIDFromBytes(decodeB64(t, vector.OperationIDB64))
	if err != nil {
		t.Fatalf("decode operation identity: %v", err)
	}
	base := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: vector.LaneID, LaneEpoch: vector.LaneEpoch, Direction: DirectionSenderToReceiver,
	}
	signingKey := vectorSenderSigningKey()
	authenticator, err := NewSenderControlAuthenticator(signingKey.Public().(ed25519.PublicKey), base, nil)
	if err != nil {
		t.Fatalf("create sender control authenticator: %v", err)
	}
	sequence := parseSequence(t, vector.Sequence)
	binding := base
	binding.Sequence = sequence
	binding.MessageKind = MessageOperationError
	binding.OperationID = operationID
	binding.HasOperationID = true
	unsigned, err := EncodeOperationFailure(OperationFailure{
		Scope: OperationScopePeer, Code: PeerOperationCodeNegotiation, Message: "Peer negotiation failed",
	})
	if err != nil {
		t.Fatalf("encode operation failure: %v", err)
	}
	signed, err := SignControlBody(signingKey, ControlDomainOperation, binding, unsigned)
	if err != nil {
		t.Fatalf("sign operation control: %v", err)
	}
	message, err := NewMessage(MessageOperationError, &operationID, signed)
	if err != nil {
		t.Fatalf("build signed operation message: %v", err)
	}
	if err := authenticator.Verify(sequence, message); err != nil {
		t.Fatalf("verify sender control: %v", err)
	}
	if err := authenticator.Verify(sequence+1, message); !errors.Is(err, ErrControlSignature) {
		t.Fatalf("wrong envelope sequence: got %v", err)
	}
	unsignedMessage, err := NewMessage(MessageOperationError, &operationID, unsigned)
	if err != nil {
		t.Fatalf("build unsigned operation message: %v", err)
	}
	if err := authenticator.Verify(sequence, unsignedMessage); !errors.Is(err, ErrControlBody) {
		t.Fatalf("capability-holder control: got %v", err)
	}

	cancel, err := NewMessage(MessageCancel, &operationID, mustControlBody(t, map[uint64]any{0: uint64(1)}))
	if err != nil {
		t.Fatalf("build direction-invalid cancel: %v", err)
	}
	if err := authenticator.Verify(sequence, cancel); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("unsigned sender cancel: got %v", err)
	}
	fragment := Message{kind: MessageBlockFragment}
	if err := authenticator.Verify(sequence, fragment); err != nil {
		t.Fatalf("binary fragment should use object authentication: %v", err)
	}
	if err := (*SenderControlAuthenticator)(nil).Verify(sequence, message); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("nil authenticator: got %v", err)
	}
}

func TestSenderControlAuthenticatorOwnsAndValidatesItsFixedBase(t *testing.T) {
	vector, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	base := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: vector.LaneID, LaneEpoch: vector.LaneEpoch, Direction: DirectionSenderToReceiver,
	}
	publicKey := vectorSenderSigningKey().Public().(ed25519.PublicKey)
	authenticator, err := NewSenderControlAuthenticator(publicKey, base, nil)
	if err != nil {
		t.Fatalf("create sender control authenticator: %v", err)
	}
	publicKey[0] ^= 1
	if bytes.Equal(authenticator.senderPublicKey, publicKey) {
		t.Fatal("authenticator retained caller-owned sender key")
	}

	invalidBases := []ControlBinding{
		{},
		bindingWith(base, func(value *ControlBinding) { value.Direction = DirectionReceiverToSender }),
		bindingWith(base, func(value *ControlBinding) { value.Sequence = 1 }),
		bindingWith(base, func(value *ControlBinding) { value.MessageKind = MessageOperationError }),
		bindingWith(base, func(value *ControlBinding) { value.HasOperationID = true }),
	}
	for index, invalid := range invalidBases {
		if _, err := NewSenderControlAuthenticator(authenticator.senderPublicKey, invalid, nil); !errors.Is(err, ErrControlBinding) {
			t.Fatalf("invalid base %d: got %v", index, err)
		}
	}
	if _, err := NewSenderControlAuthenticator(authenticator.senderPublicKey[:ed25519.PublicKeySize-1], base, nil); !errors.Is(err, ErrControlSigningKey) {
		t.Fatalf("short sender key: got %v", err)
	}
}

func vectorSenderSigningKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(sequentialBytes(0x20, ed25519.SeedSize))
}

func parseSequence(t *testing.T, value string) uint64 {
	t.Helper()
	sequence, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("parse sequence %q: %v", value, err)
	}
	return sequence
}

func mustControlBody(t *testing.T, fields map[uint64]any) []byte {
	t.Helper()
	encoded, err := messageEncMode.Marshal(fields)
	if err != nil {
		t.Fatalf("encode control body: %v", err)
	}
	return encoded
}

func mustSignedControlBody(t *testing.T, signed, replacementUnsigned []byte) []byte {
	t.Helper()
	_, signature, err := decodeSignedControlBody(signed)
	if err != nil {
		t.Fatalf("decode signed control body: %v", err)
	}
	encoded, err := encodeSignedControlWrapper(replacementUnsigned, signature)
	if err != nil {
		t.Fatalf("encode substituted control body: %v", err)
	}
	return encoded
}

func bindingWith(binding ControlBinding, mutate func(*ControlBinding)) ControlBinding {
	mutate(&binding)
	return binding
}

func TestControlSignedBodyOwnsSignatureBytes(t *testing.T) {
	// This small ownership check catches accidental use of decoder-backed slices
	// when verification returns the unsigned body to the router.
	vector, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, _ := OperationIDFromBytes(decodeB64(t, vector.OperationIDB64))
	binding := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: vector.LaneID, Direction: DirectionSenderToReceiver, MessageKind: MessageOperationError,
		OperationID: operationID, HasOperationID: true,
	}
	unsigned := decodeB64(t, vector.UnsignedControlB64)
	signed, err := SignControlBody(vectorSenderSigningKey(), ControlDomainOperation, binding, unsigned)
	if err != nil {
		t.Fatalf("sign control body: %v", err)
	}
	verified, err := VerifyControlBody(vectorSenderSigningKey().Public().(ed25519.PublicKey), ControlDomainOperation, binding, signed)
	if err != nil {
		t.Fatalf("verify control body: %v", err)
	}
	verified[0] ^= 1
	if bytes.Equal(verified, unsigned) || !bytes.Equal(unsigned, decodeB64(t, vector.UnsignedControlB64)) {
		t.Fatal("control verification did not return caller-independent bytes")
	}
}

func TestSenderControlSemanticBodyRemovesOnlyAuthenticatedDeliveryField(t *testing.T) {
	vector, _, _ := loadEnvelopeVector(t, "sender-signed-operation-error")
	message, err := DecodeMessage(decodeB64(t, vector.PlaintextB64))
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := SenderControlSemanticBody(message)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unsigned, decodeB64(t, vector.SemanticBodyB64)) {
		t.Fatal("signature removal changed the semantic control body")
	}
	if _, err := SenderControlSemanticBody(Message{kind: MessageListChildren}); !errors.Is(err, ErrControlBinding) {
		t.Fatalf("receiver-authored message error = %v", err)
	}
	if _, err := SenderControlSemanticBody(Message{kind: MessageOperationError, body: unsigned}); !errors.Is(err, ErrControlBody) {
		t.Fatalf("unsigned sender message error = %v", err)
	}
}
