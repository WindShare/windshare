package protocolsession

import (
	"bytes"
	"errors"
	"testing"
)

func TestFixedIdentitiesOwnTheirBytes(t *testing.T) {
	raw := sequentialBytes(0x10, IdentityBytes)
	session, err := ProtocolSessionIDFromBytes(raw)
	if err != nil {
		t.Fatalf("create protocol session identity: %v", err)
	}
	receiver, err := ReceiverInstanceIDFromBytes(raw)
	if err != nil {
		t.Fatalf("create receiver identity: %v", err)
	}
	operationRaw := sequentialBytes(0x10, OperationIDBytes)
	operation, err := OperationIDFromBigEndian(operationRaw)
	if err != nil {
		t.Fatalf("create operation identity: %v", err)
	}

	raw[0] ^= 0xff
	operationRaw[0] ^= 0xff
	var encoded [OperationIDBytes]byte
	operation.PutBigEndian(encoded[:])
	if session.Bytes()[0] != 0x10 || receiver.Bytes()[0] != 0x10 || encoded[0] != 0x10 {
		t.Fatal("identity retained caller-owned input")
	}
	copyOut := session.Bytes()
	copyOut[0] ^= 0xff
	if !bytes.Equal(session.Bytes(), receiver.Bytes()) || !operation.Equal(OperationID(0x1011121314151617)) {
		t.Fatal("identity accessors did not preserve value semantics")
	}
	if session.IsZero() || receiver.IsZero() || operation.IsZero() {
		t.Fatal("nonzero identity reported zero")
	}
	if operation.String() != "1157726452361532951" {
		t.Fatalf("operation.String() = %q", operation.String())
	}
}

func TestFixedIdentitiesRejectEveryOtherLength(t *testing.T) {
	constructors := []struct {
		name string
		call func([]byte) error
	}{
		{"protocol session", func(raw []byte) error { _, err := ProtocolSessionIDFromBytes(raw); return err }},
		{"receiver", func(raw []byte) error { _, err := ReceiverInstanceIDFromBytes(raw); return err }},
	}
	for _, constructor := range constructors {
		t.Run(constructor.name, func(t *testing.T) {
			for _, size := range []int{0, IdentityBytes - 1, IdentityBytes + 1, 64} {
				if err := constructor.call(make([]byte, size)); !errors.Is(err, ErrIdentityLength) {
					t.Fatalf("size %d: got %v, want ErrIdentityLength", size, err)
				}
			}
		})
	}
	for _, size := range []int{0, OperationIDBytes - 1, OperationIDBytes + 1, IdentityBytes} {
		if _, err := OperationIDFromBigEndian(make([]byte, size)); !errors.Is(err, ErrIdentityLength) {
			t.Fatalf("operation size %d: got %v, want ErrIdentityLength", size, err)
		}
	}
	if _, err := OperationIDFromBigEndian(make([]byte, OperationIDBytes)); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("zero big-endian operation ID: got %v, want ErrInvalidOperationID", err)
	}
	if _, err := OperationIDFromUint64(0); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("zero uint64 operation ID: got %v, want ErrInvalidOperationID", err)
	}

	if !(ProtocolSessionID{}).IsZero() || !(ReceiverInstanceID{}).IsZero() || !OperationID(0).IsZero() {
		t.Fatal("zero-value identity must remain recognizable")
	}
}
