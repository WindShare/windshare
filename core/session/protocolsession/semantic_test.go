package protocolsession

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSenderControlSemanticRegistryFailsClosed(t *testing.T) {
	valid := SenderControlSemanticValidatorFunc(func(MessageKind, OperationID, []byte) error { return nil })
	for name, rules := range map[string][]SenderControlSemanticRule{
		"empty":         nil,
		"nil validator": {{Kind: MessageCatalogResult}},
		"receiver kind": {{Kind: MessageListChildren, Validate: valid}},
		"duplicate kind": {
			{Kind: MessageCatalogResult, Validate: valid},
			{Kind: MessageCatalogResult, Validate: valid},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSenderControlSemanticRegistry(rules...); !errors.Is(err, ErrControlSemantic) {
				t.Fatalf("registry error = %v, want %v", err, ErrControlSemantic)
			}
		})
	}

	var nilFunc SenderControlSemanticValidatorFunc
	if err := nilFunc.ValidateSenderControl(MessageCatalogResult, OperationID{}, nil); !errors.Is(err, ErrControlSemantic) {
		t.Fatalf("nil validator function error = %v", err)
	}
	var nilRegistry *SenderControlSemanticRegistry
	if err := nilRegistry.ValidateSenderControl(MessageCatalogResult, OperationID{}, nil); !errors.Is(err, ErrControlSemantic) {
		t.Fatalf("nil registry error = %v", err)
	}
}

func TestSenderControlSemanticRegistryDispatchesOnlyRegisteredKind(t *testing.T) {
	operationID := OperationID{1}
	semantic := []byte{2}
	called := false
	registry, err := NewSenderControlSemanticRegistry(SenderControlSemanticRule{
		Kind: MessageCatalogResult,
		Validate: func(kind MessageKind, gotOperationID OperationID, gotSemantic []byte) error {
			called = true
			if kind != MessageCatalogResult || gotOperationID != operationID || len(gotSemantic) != 1 || gotSemantic[0] != 2 {
				t.Fatalf("validator arguments = kind %d operation %x semantic %x", kind, gotOperationID, gotSemantic)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateSenderControl(MessageCatalogResult, operationID, semantic); err != nil || !called {
		t.Fatalf("registered validation = called %v err %v", called, err)
	}
	if err := registry.ValidateSenderControl(MessageOpenResults, operationID, semantic); !errors.Is(err, ErrControlSemantic) {
		t.Fatalf("unregistered validation error = %v", err)
	}
}

func TestValidateSenderControlSemanticOwnsCoreSchemas(t *testing.T) {
	failure, err := EncodeOperationFailure(OperationFailure{
		Scope: OperationScopePeer, Code: PeerOperationCodeNegotiation, Message: "Peer negotiation failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := EncodeScanProgress(ScanProgress{
		AttemptID: catalog.ScanAttemptID{1}, DiscoveredEntries: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := EncodeSessionTerminal(SessionTerminal{
		Code: SessionTerminalCodeFirst, Message: "Sender stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	externalCalled := false
	external := SenderControlSemanticValidatorFunc(func(MessageKind, OperationID, []byte) error {
		externalCalled = true
		return nil
	})
	for kind, body := range map[MessageKind][]byte{
		MessageOperationError:  failure,
		MessageScanProgress:    progress,
		MessageSessionTerminal: terminal,
	} {
		if err := validateSenderControlSemantic(external, kind, OperationID{2}, body); err != nil {
			t.Fatalf("valid core kind %d: %v", kind, err)
		}
		if err := validateSenderControlSemantic(external, kind, OperationID{2}, []byte{0xf6}); !errors.Is(err, ErrControlSemantic) {
			t.Fatalf("malformed core kind %d error = %v", kind, err)
		}
	}
	if externalCalled {
		t.Fatal("core schema validation delegated outside protocolsession")
	}
}

func TestValidateSenderControlSemanticDelegatesNonCoreKinds(t *testing.T) {
	operationID := OperationID{3}
	semantic := []byte{4}
	sentinel := errors.New("typed decoder rejected body")
	external := SenderControlSemanticValidatorFunc(func(
		kind MessageKind,
		gotOperationID OperationID,
		gotSemantic []byte,
	) error {
		if kind != MessageCatalogResult || gotOperationID != operationID || len(gotSemantic) != 1 || gotSemantic[0] != 4 {
			t.Fatalf("delegated arguments = kind %d operation %x semantic %x", kind, gotOperationID, gotSemantic)
		}
		return sentinel
	})
	if err := validateSenderControlSemantic(external, MessageCatalogResult, operationID, semantic); !errors.Is(err, ErrControlSemantic) || !errors.Is(err, sentinel) {
		t.Fatalf("delegated error = %v", err)
	}
	if err := validateSenderControlSemantic(nil, MessageCatalogResult, operationID, semantic); !errors.Is(err, ErrControlSemantic) {
		t.Fatalf("missing external validator error = %v", err)
	}
}
