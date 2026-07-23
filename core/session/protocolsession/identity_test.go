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
	operation, err := OperationIDFromBytes(raw)
	if err != nil {
		t.Fatalf("create operation identity: %v", err)
	}

	raw[0] ^= 0xff
	if session.Bytes()[0] != 0x10 || receiver.Bytes()[0] != 0x10 || operation.Bytes()[0] != 0x10 {
		t.Fatal("identity retained caller-owned input")
	}
	copyOut := session.Bytes()
	copyOut[0] ^= 0xff
	if !bytes.Equal(session.Bytes(), receiver.Bytes()) || !operation.Equal(OperationID(receiver)) {
		t.Fatal("identity accessors did not preserve value semantics")
	}
	if session.IsZero() || receiver.IsZero() || operation.IsZero() {
		t.Fatal("nonzero identity reported zero")
	}
}

func TestFixedIdentitiesRejectEveryOtherLength(t *testing.T) {
	constructors := []struct {
		name string
		call func([]byte) error
	}{
		{"protocol session", func(raw []byte) error { _, err := ProtocolSessionIDFromBytes(raw); return err }},
		{"receiver", func(raw []byte) error { _, err := ReceiverInstanceIDFromBytes(raw); return err }},
		{"operation", func(raw []byte) error { _, err := OperationIDFromBytes(raw); return err }},
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

	if !(ProtocolSessionID{}).IsZero() || !(ReceiverInstanceID{}).IsZero() || !(OperationID{}).IsZero() {
		t.Fatal("zero-value identity must remain recognizable")
	}
}
