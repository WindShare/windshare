package protocolsession

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

type envelopeVector struct {
	Name                 string `json:"name"`
	AADB64               string `json:"aadB64"`
	ControlPreimageB64   string `json:"controlPreimageB64"`
	ControlSignatureB64  string `json:"controlSignatureB64"`
	EnvelopeB64          string `json:"envelopeB64"`
	LaneEpoch            uint32 `json:"laneEpoch"`
	LaneID               uint32 `json:"laneId"`
	NonceB64             string `json:"nonceB64"`
	OperationIDB64       string `json:"operationIdB64"`
	PlaintextB64         string `json:"plaintextB64"`
	ProtocolSessionIDB64 string `json:"protocolSessionIdB64"`
	Sequence             string `json:"sequence"`
	SemanticBodyB64      string `json:"semanticBodyCborB64"`
	ShareInstanceB64     string `json:"shareInstanceB64"`
	SignedControlB64     string `json:"signedControlCborB64"`
	TrafficKeyB64        string `json:"trafficKeyB64"`
	UnsignedControlB64   string `json:"unsignedControlCborB64"`
}

func TestEnvelopeMatchesGoldenSessionVector(t *testing.T) {
	vector, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	nonce := decodeB64(t, vector.NonceB64)
	plaintext := decodeB64(t, vector.PlaintextB64)
	sealer, err := NewEnvelopeSealer(key, binding, bytes.NewReader(nonce))
	if err != nil {
		t.Fatalf("create envelope sealer: %v", err)
	}
	sealed, err := sealer.Seal(plaintext)
	if err != nil {
		t.Fatalf("seal golden envelope: %v", err)
	}
	if sealed.Sequence != 0 {
		t.Fatalf("sealed sequence = %d, want 0", sealed.Sequence)
	}
	assertBytes(t, "envelope AAD", envelopeAAD(binding, 0, uint32(len(plaintext)+EnvelopeTagBytes)), decodeB64(t, vector.AADB64))
	assertBytes(t, "operation envelope", sealed.Frame, decodeB64(t, vector.EnvelopeB64))

	opener, err := NewEnvelopeOpener(key, binding)
	if err != nil {
		t.Fatalf("create envelope opener: %v", err)
	}
	opened, err := opener.Open(sealed.Frame)
	if err != nil {
		t.Fatalf("open golden envelope: %v", err)
	}
	if opened.Sequence != 0 {
		t.Fatalf("opened sequence = %d, want 0", opened.Sequence)
	}
	assertBytes(t, "opened plaintext", opened.Plaintext, plaintext)
	if _, err := opener.Open(sealed.Frame); !errors.Is(err, ErrEnvelopeSequence) {
		t.Fatalf("replay: got %v, want ErrEnvelopeSequence", err)
	}
}

func TestEnvelopeAuthenticationBindsEverySessionAxis(t *testing.T) {
	vector, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	frame := decodeB64(t, vector.EnvelopeB64)

	tampered := append([]byte(nil), frame...)
	tampered[len(tampered)-1] ^= 1
	opener := mustOpener(t, key, binding)
	if _, err := opener.Open(tampered); !errors.Is(err, ErrEnvelopeAuthentication) {
		t.Fatalf("tampered ciphertext: got %v", err)
	}
	if next, err := opener.NextSequence(); err != nil || next != 0 {
		t.Fatalf("failed authentication consumed sequence: next=%d err=%v", next, err)
	}
	if _, err := opener.Open(frame); err != nil {
		t.Fatalf("valid frame after forgery: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*EnvelopeBinding)
		want   error
	}{
		{"share", func(value *EnvelopeBinding) { value.ShareInstance[0] ^= 1 }, ErrEnvelopeAuthentication},
		{"protocol session", func(value *EnvelopeBinding) { value.ProtocolSessionID[0] ^= 1 }, ErrEnvelopeAuthentication},
		{"lane", func(value *EnvelopeBinding) { value.LaneID++ }, ErrEnvelopeAuthentication},
		{"epoch", func(value *EnvelopeBinding) { value.LaneEpoch++ }, ErrEnvelopeAuthentication},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := binding
			mutation.mutate(&changed)
			if _, err := mustOpener(t, key, changed).Open(frame); !errors.Is(err, mutation.want) {
				t.Fatalf("got %v, want %v", err, mutation.want)
			}
		})
	}
	wrongDirection, err := TrafficKeyFromBytes(key.Bytes(), DirectionReceiverToSender)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnvelopeOpener(wrongDirection, binding); !errors.Is(err, ErrTrafficKeyDirection) {
		t.Fatalf("swapped directional key: got %v", err)
	}
	if _, err := NewEnvelopeSealer(wrongDirection, binding, bytes.NewReader(make([]byte, EnvelopeNonceBytes))); !errors.Is(err, ErrTrafficKeyDirection) {
		t.Fatalf("sealer accepted swapped directional key: got %v", err)
	}
	changedDirection := binding
	changedDirection.Direction = DirectionReceiverToSender
	if _, err := NewEnvelopeOpener(key, changedDirection); !errors.Is(err, ErrTrafficKeyDirection) {
		t.Fatalf("binding/key direction mismatch: got %v", err)
	}
	if _, err := NewEnvelopeOpener(TrafficKey{}, binding); !errors.Is(err, ErrTrafficKeyDirection) {
		t.Fatalf("zero traffic key: got %v", err)
	}
	invalidBinding := binding
	invalidBinding.ProtocolSessionID = ProtocolSessionID{}
	if _, err := NewEnvelopeSealer(key, invalidBinding, bytes.NewReader(make([]byte, EnvelopeNonceBytes))); !errors.Is(err, ErrEnvelopeBinding) {
		t.Fatalf("sealer accepted invalid binding: got %v", err)
	}
}

func TestEnvelopeRejectsMalformedHeaderBeforeDecrypting(t *testing.T) {
	vector, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	valid := decodeB64(t, vector.EnvelopeB64)
	mutate := func(change func([]byte)) []byte {
		frame := append([]byte(nil), valid...)
		change(frame)
		return frame
	}
	tests := []struct {
		name  string
		frame []byte
		want  error
	}{
		{"short", valid[:EnvelopeOverheadBytes-1], ErrEnvelopeMalformed},
		{"oversize", make([]byte, MaxEnvelopeBytes+1), ErrEnvelopeTooLarge},
		{"version", mutate(func(frame []byte) { frame[0]++ }), ErrEnvelopeVersion},
		{"unknown flag", mutate(func(frame []byte) { frame[1] = 2 }), ErrEnvelopeMalformed},
		{"reserved", mutate(func(frame []byte) { frame[2] = 1 }), ErrEnvelopeMalformed},
		{"short ciphertext", mutate(func(frame []byte) { binary.BigEndian.PutUint32(frame[12:16], EnvelopeTagBytes-1) }), ErrEnvelopeMalformed},
		{"declared length mismatch", mutate(func(frame []byte) { binary.BigEndian.PutUint32(frame[12:16], uint32(len(frame)-EnvelopeHeaderBytes-1)) }), ErrEnvelopeMalformed},
		{"trailing byte", append(append([]byte(nil), valid...), 0), ErrEnvelopeMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mustOpener(t, key, binding).Open(test.frame); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestEnvelopeSequenceIsStrictAndDoesNotWrap(t *testing.T) {
	_, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	nonces := sequentialBytes(0x10, EnvelopeNonceBytes*2)
	sealer, err := NewEnvelopeSealer(key, binding, bytes.NewReader(nonces))
	if err != nil {
		t.Fatalf("create sealer: %v", err)
	}
	first, err := sealer.Seal([]byte("first"))
	if err != nil {
		t.Fatalf("seal first frame: %v", err)
	}
	second, err := sealer.Seal([]byte("second"))
	if err != nil {
		t.Fatalf("seal second frame: %v", err)
	}
	opener := mustOpener(t, key, binding)
	if _, err := opener.Open(second.Frame); !errors.Is(err, ErrEnvelopeSequence) {
		t.Fatalf("sequence skip: got %v", err)
	}
	if _, err := opener.Open(first.Frame); err != nil {
		t.Fatalf("open first after rejected skip: %v", err)
	}
	if _, err := opener.Open(first.Frame); !errors.Is(err, ErrEnvelopeSequence) {
		t.Fatalf("sequence replay: got %v", err)
	}
	if _, err := opener.Open(second.Frame); err != nil {
		t.Fatalf("open second: %v", err)
	}

	maxSealer, err := NewEnvelopeSealer(key, binding, bytes.NewReader(make([]byte, EnvelopeNonceBytes)))
	if err != nil {
		t.Fatalf("create max-sequence sealer: %v", err)
	}
	maxSealer.next = math.MaxUint64
	last, err := maxSealer.Seal(nil)
	if err != nil {
		t.Fatalf("seal maximum sequence: %v", err)
	}
	if last.Sequence != math.MaxUint64 {
		t.Fatalf("last sequence = %d", last.Sequence)
	}
	if _, err := maxSealer.Seal(nil); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("sealer wrap: got %v", err)
	}
	maxOpener := mustOpener(t, key, binding)
	maxOpener.next = math.MaxUint64
	if _, err := maxOpener.Open(last.Frame); err != nil {
		t.Fatalf("open maximum sequence: %v", err)
	}
	if _, err := maxOpener.Open(last.Frame); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("opener wrap: got %v", err)
	}
}

func TestEnvelopeLimitsAndNonceFailureAreFailClosed(t *testing.T) {
	_, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	if size, err := EnvelopeFrameSize(MaxEnvelopePlaintextBytes); err != nil || size != MaxEnvelopeBytes {
		t.Fatalf("maximum frame size = %d, err=%v", size, err)
	}
	for _, size := range []int{-1, MaxEnvelopePlaintextBytes + 1} {
		if _, err := EnvelopeFrameSize(size); !errors.Is(err, ErrEnvelopeTooLarge) {
			t.Fatalf("plaintext size %d: got %v", size, err)
		}
	}
	sealer, err := NewEnvelopeSealer(key, binding, bytes.NewReader(make([]byte, EnvelopeNonceBytes)))
	if err != nil {
		t.Fatalf("create maximum sealer: %v", err)
	}
	sealed, err := sealer.Seal(make([]byte, MaxEnvelopePlaintextBytes))
	if err != nil || len(sealed.Frame) != MaxEnvelopeBytes {
		t.Fatalf("seal maximum frame: len=%d err=%v", len(sealed.Frame), err)
	}
	if _, err := sealer.Seal(make([]byte, MaxEnvelopePlaintextBytes+1)); !errors.Is(err, ErrEnvelopeTooLarge) {
		t.Fatalf("oversize plaintext: got %v", err)
	}

	failing, err := NewEnvelopeSealer(key, binding, bytes.NewReader(make([]byte, EnvelopeNonceBytes-1)))
	if err != nil {
		t.Fatalf("create short-source sealer: %v", err)
	}
	if _, err := failing.Seal([]byte("payload")); !errors.Is(err, ErrNonceSource) {
		t.Fatalf("short nonce source: got %v", err)
	}
	if _, err := failing.NextSequence(); !errors.Is(err, ErrNonceSource) || failing.next != 0 {
		t.Fatalf("nonce failure consumed sequence: next=%d err=%v", failing.next, err)
	}
	if _, err := NewEnvelopeSealer(key, binding, nil); !errors.Is(err, ErrNonceSource) {
		t.Fatalf("nil nonce source: got %v", err)
	}
	invalid := binding
	invalid.LaneID = 0
	if _, err := NewEnvelopeOpener(key, invalid); !errors.Is(err, ErrEnvelopeBinding) {
		t.Fatalf("invalid binding: got %v", err)
	}
}

func loadEnvelopeVector(t *testing.T, name string) (envelopeVector, TrafficKey, EnvelopeBinding) {
	t.Helper()
	var vector envelopeVector
	decodeSessionCase(t, name, &vector)
	share := mustShare(t, decodeB64(t, vector.ShareInstanceB64))
	sessionID, err := ProtocolSessionIDFromBytes(decodeB64(t, vector.ProtocolSessionIDB64))
	if err != nil {
		t.Fatalf("decode vector protocol session identity: %v", err)
	}
	key, err := TrafficKeyFromBytes(decodeB64(t, vector.TrafficKeyB64), DirectionSenderToReceiver)
	if err != nil {
		t.Fatalf("decode vector traffic key: %v", err)
	}
	return vector, key, EnvelopeBinding{
		ShareInstance: share, ProtocolSessionID: sessionID, LaneID: vector.LaneID,
		LaneEpoch: vector.LaneEpoch, Direction: DirectionSenderToReceiver,
	}
}

func mustOpener(t *testing.T, key TrafficKey, binding EnvelopeBinding) *EnvelopeOpener {
	t.Helper()
	opener, err := NewEnvelopeOpener(key, binding)
	if err != nil {
		t.Fatalf("create opener: %v", err)
	}
	return opener
}
