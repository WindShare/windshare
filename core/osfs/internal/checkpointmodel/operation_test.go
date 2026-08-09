package checkpointmodel

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReceiveOperationRoundTripRemainsOpaqueUntilIntentVerification(t *testing.T) {
	intent, authority := receiveOperationIntentFixture(t, 0x31)
	key, err := NewCLICompatibleOperationKey(intent.SelectionSpec(), intent.ArtifactSpec(), authority)
	if err != nil {
		t.Fatal(err)
	}
	reopen, err := CLIReopenKey(key)
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewReceiveOperation(intent, reopen)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeReceiveOperation(record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeReceiveOperation(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if restored.OperationID() != intent.OperationID() ||
		restored.ReceiveIntentDigest() != intent.Digest() ||
		restored.BindingDigest() != intent.BindingDigest() ||
		restored.ReopenKey().CompatibleKey() != key {
		t.Fatal("operation record lost an immutable identity")
	}
	decoded, err := restored.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil || !decoded.EqualCanonical(intent) {
		t.Fatalf("verified intent = %v", err)
	}

	corrupt := append([]byte(nil), encoded...)
	corrupt[len(receiveOperationDomain)+2+8+receivecontract.StableIdentityBytes+8] ^= 1
	if _, err := DecodeReceiveOperation(corrupt); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("corrupt operation error = %v", err)
	}
	if _, err := restored.VerifyIntent(func([]byte) (transfer.ReceiveIntent, error) {
		foreign, _ := receiveOperationIntentFixture(t, 0x41)
		return foreign, nil
	}); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("foreign intent verification error = %v", err)
	}
}

func TestReceiveOperationRejectsOpenUnionsAndMalformedFrames(t *testing.T) {
	intent, _ := receiveOperationIntentFixture(t, 0x51)
	record, err := NewReceiveOperation(intent, NoReopenKey())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeReceiveOperation(record)
	if err != nil {
		t.Fatal(err)
	}
	if record.ReopenKey().Kind() != ReopenNone || !record.ReopenKey().CompatibleKey().IsZero() {
		t.Fatal("none reopen key carried a hidden compatible key")
	}
	for name, value := range map[string][]byte{
		"empty":      nil,
		"truncated":  encoded[:len(encoded)-1],
		"foreign":    append([]byte("foreign"), encoded...),
		"trailing":   append(append([]byte(nil), encoded...), 0),
		"open-union": func() []byte { value := append([]byte(nil), encoded...); value[len(value)-1] = 99; return value }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeReceiveOperation(value); !errors.Is(err, ErrInvalidReceiveOperation) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
	if _, err := CompatibleOperationKeyFromBytes(make([]byte, 32)); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("zero compatible key error = %v", err)
	}
	if _, err := CLIReopenKey(CompatibleOperationKey{}); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("zero CLI reopen key error = %v", err)
	}
}

func receiveOperationIntentFixture(
	t *testing.T,
	seed byte,
) (transfer.ReceiveIntent, receivecontract.AuthorityRef) {
	t.Helper()
	var share catalog.ShareInstance
	var root catalog.DirectoryID
	share[0], root[0] = seed, seed+1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, err := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{seed + 2}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		bytes.Repeat([]byte{seed + 3}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{seed + 4}, receivecontract.AuthorityRefBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent, authority
}
