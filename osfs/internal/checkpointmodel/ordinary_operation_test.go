package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestActiveOperationKeyV1FreezesExactDomainInputs(t *testing.T) {
	intent, _ := ordinaryOperationIntentFixture(t, 0x21)
	authority, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x72}, receivecontract.AuthorityRefBytes))
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("windshare/active-operation-key/v1"))
	_, _ = hash.Write([]byte{0, 1})
	writeOrdinaryFrame(hash, intent.SelectionSpec().Digest().Bytes())
	writeOrdinaryFrame(hash, authority.Bytes())
	writeOrdinaryFrame(hash, []byte{1})
	if !bytes.Equal(key.Bytes(), hash.Sum(nil)) || ActiveOperationKeyDomainV1 != "windshare/active-operation-key/v1" ||
		ActiveOperationKeyV1 != 1 || ordinaryoutput.OrdinaryOutputPolicyVersion != 1 {
		t.Fatalf("active key/domain/version drifted: %x/%q/%d", key.Bytes(), ActiveOperationKeyDomainV1, ActiveOperationKeyV1)
	}
	otherPolicy, err := NewActiveOperationKey(intent.SelectionSpec().Digest(), authority, 2)
	if err != nil || otherPolicy == key {
		t.Fatal("policy version did not separate active operations")
	}
	otherIntent, _ := ordinaryOperationIntentFixture(t, 0x31)
	otherSelection, _ := NewActiveOperationKeyV1(otherIntent.SelectionSpec().Digest(), authority)
	otherAuthority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x73}, 32))
	otherDestination, _ := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), otherAuthority)
	if otherSelection == key || otherDestination == key {
		t.Fatal("active key omitted selection or destination authority")
	}
	// Artifact, operation ID, reservation, and intent digest are deliberately
	// unavailable to this constructor: only the frozen selection digest crosses
	// the active-lookup boundary.
	repeated, err := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	if err != nil || repeated != key {
		t.Fatal("active key depended on shape-time identity")
	}
}

func TestOrdinaryOperationRecordIsBoundedAndCanonical(t *testing.T) {
	intent, _ := ordinaryOperationIntentFixture(t, 0x41)
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x81}, 32))
	key, _ := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	record, err := NewOrdinaryOperationRecord(OrdinaryOperationRecordSpec{
		ActiveKey: key, Intent: intent, LifecycleGeneration: 1,
		ReservationClaim: ordinaryClaimLocatorFixture(t, 0xa1),
		Lifecycle:        OrdinaryOperationActive, Lease: OrdinaryLeaseHeld,
		ClosedReason: OrdinaryReasonNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeOrdinaryOperationRecord(record)
	if err != nil || len(encoded) > MaximumOrdinaryOperationRecordBytesV1 {
		t.Fatalf("encode = %d bytes, %v", len(encoded), err)
	}
	if OrdinaryOperationRecordDomainV1 != "windshare/ordinary-operation/v1" || OrdinaryOperationRecordVersionV1 != 1 ||
		MaximumOrdinaryOperationRecordBytesV1 != 2*1024*1024 || MaximumOrdinaryReceiveIntentBytesV1 != 1024*1024 {
		t.Fatal("ordinary operation schema constants drifted")
	}
	restored, err := DecodeOrdinaryOperationRecord(encoded)
	if err != nil || restored.ActiveOperationKey() != key || restored.OperationID() != intent.OperationID() ||
		restored.ReceiveIntentDigest() != intent.Digest() || restored.Lifecycle() != OrdinaryOperationActive ||
		restored.Lease() != OrdinaryLeaseHeld {
		t.Fatalf("restored operation = %+v, %v", restored, err)
	}
	if restored.ReservationClaim() != record.ReservationClaim() {
		t.Fatal("reservation claim locator was not authenticated by the row")
	}
	binding, err := OrdinaryOperationBindingDigest(restored)
	if err != nil || binding == ([sha256.Size]byte{}) {
		t.Fatalf("operation binding digest = %x, %v", binding, err)
	}
	verified, err := restored.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil || !verified.EqualCanonical(intent) {
		t.Fatalf("verified intent = %v", err)
	}
	returned := restored.IntentBytes()
	returned[0] ^= 0xff
	if bytes.Equal(returned, restored.IntentBytes()) {
		t.Fatal("operation record exposed mutable canonical intent storage")
	}
	for name, invalid := range map[string][]byte{
		"empty":          nil,
		"truncated":      encoded[:len(encoded)-1],
		"trailing":       append(append([]byte(nil), encoded...), 0),
		"foreign domain": append([]byte("foreign"), encoded...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOrdinaryOperationRecord(invalid); !errors.Is(err, ErrInvalidOrdinaryOperation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOrdinaryOperationRecordClosesReasonByLifecycle(t *testing.T) {
	intent, _ := ordinaryOperationIntentFixture(t, 0x51)
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x91}, 32))
	key, _ := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	base := OrdinaryOperationRecordSpec{
		ActiveKey: key, Intent: intent, LifecycleGeneration: 1,
		ReservationClaim: ordinaryClaimLocatorFixture(t, 0xb1),
		Lifecycle:        OrdinaryOperationActive, Lease: OrdinaryLeaseReleased,
		ClosedReason: OrdinaryReasonNone,
	}
	if _, err := NewOrdinaryOperationRecord(base); err != nil {
		t.Fatal(err)
	}
	base.ClosedReason = OrdinaryReasonRegistryOwnershipUnknown
	if _, err := NewOrdinaryOperationRecord(base); !errors.Is(err, ErrInvalidOrdinaryOperation) {
		t.Fatalf("active reason error = %v", err)
	}
	base.Lifecycle = OrdinaryOperationNeedsAttention
	if _, err := NewOrdinaryOperationRecord(base); err != nil {
		t.Fatalf("attention record error = %v", err)
	}
	base.Lifecycle, base.ClosedReason = OrdinaryOperationCleanupPending, OrdinaryReasonCleanupUncertain
	if _, err := NewOrdinaryOperationRecord(base); err != nil {
		t.Fatalf("cleanup-pending record error = %v", err)
	}
}

func TestOrdinaryAdmissionCandidateCanonicalTransitions(t *testing.T) {
	intent, _ := ordinaryOperationIntentFixture(t, 0x61)
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x92}, 32))
	key, _ := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	candidate, err := NewOrdinaryAdmissionCandidate(key, intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	locator := ordinaryClaimLocatorFixture(t, 0xc1)
	bound, err := BindOrdinaryAdmissionReservation(candidate, locator)
	if err != nil || bound.ReservationClaim() != locator || bound.Generation() != 2 {
		t.Fatalf("bind candidate = %+v, %v", bound, err)
	}
	attention, err := RequireOrdinaryAdmissionAttention(bound)
	if err != nil || attention.State() != OrdinaryAdmissionNeedsAttention || attention.Generation() != 3 {
		t.Fatalf("attention = %+v, %v", attention, err)
	}
	encoded, err := EncodeOrdinaryAdmissionCandidate(attention)
	restored, decodeErr := DecodeOrdinaryAdmissionCandidate(encoded)
	if err != nil || decodeErr != nil || restored.ReservationClaim() != locator || restored.State() != attention.State() {
		t.Fatalf("candidate round trip = %+v, %v/%v", restored, err, decodeErr)
	}
	if _, err := DecodeOrdinaryAdmissionCandidate(append(encoded, 0)); !errors.Is(err, ErrInvalidOrdinaryOperation) {
		t.Fatalf("trailing candidate error = %v", err)
	}
}

func ordinaryClaimLocatorFixture(t *testing.T, fill byte) ReservationClaimLocator {
	t.Helper()
	var token [sha256.Size]byte
	token[0] = fill
	locator, err := NewReservationClaimLocator(token, 3)
	if err != nil {
		t.Fatal(err)
	}
	return locator
}
