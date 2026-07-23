package sessionruntime

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRemoteOperationErrorRejectsAuthenticatedMalformedSemantics(t *testing.T) {
	operationID := id16[protocolsession.OperationID](201)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{202}, ed25519.SeedSize))
	binding := protocolsession.ControlBinding{
		ShareInstance:     id16[catalog.ShareInstance](203),
		ProtocolSessionID: id16[protocolsession.ProtocolSessionID](204),
		LaneID:            205,
		Direction:         protocolsession.DirectionSenderToReceiver,
		MessageKind:       protocolsession.MessageOperationError,
		OperationID:       operationID,
		HasOperationID:    true,
	}
	valid := map[uint64]any{
		0: uint64(1),
		1: uint64(protocolsession.OperationScopeBlock),
		2: uint64(contentflow.BlockCodeInvalidRef),
		3: true,
		4: uint64(1),
		5: "retry later",
	}
	for name, mutate := range map[string]func(map[uint64]any){
		"scope":         func(fields map[uint64]any) { fields[1] = uint64(1) },
		"cross-scope":   func(fields map[uint64]any) { fields[2] = uint64(0x3001) },
		"zero-retry":    func(fields map[uint64]any) { fields[4] = uint64(0) },
		"large-retry":   func(fields map[uint64]any) { fields[4] = uint64(30_001) },
		"empty-message": func(fields map[uint64]any) { fields[5] = "" },
		"extra-field":   func(fields map[uint64]any) { fields[6] = uint64(0) },
	} {
		t.Run(name, func(t *testing.T) {
			fields := make(map[uint64]any, len(valid)+1)
			for key, value := range valid {
				fields[key] = value
			}
			mutate(fields)
			semantic, err := protocolsession.EncodeBody(fields)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := protocolsession.SignControlBody(
				privateKey, protocolsession.ControlDomainOperation, binding, semantic,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := protocolsession.VerifyControlBody(
				privateKey.Public().(ed25519.PublicKey),
				protocolsession.ControlDomainOperation,
				binding,
				signed,
			); err != nil {
				t.Fatalf("fixture signature is invalid: %v", err)
			}
			message, err := protocolsession.NewMessage(
				protocolsession.MessageOperationError, &operationID, signed,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := remoteOperationError(message); !errors.Is(err, protocolsession.ErrInvalidOperationFailure) {
				t.Fatalf("authenticated malformed operation error = %v", err)
			}
		})
	}
}
