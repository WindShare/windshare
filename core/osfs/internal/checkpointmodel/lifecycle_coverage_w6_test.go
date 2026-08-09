package checkpointmodel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

type lifecycleCoverageFixture struct {
	operation ReceiveOperation
	reference FileCheckpointReference
	evidence  AggregateDigest
	receipts  map[DirectTreeReceiptKind]DirectTreeReceipt
}

func newLifecycleCoverageFixture(t *testing.T, seed byte) lifecycleCoverageFixture {
	t.Helper()
	intent, authority := receiveOperationIntentFixture(t, seed)
	operation, err := NewReceiveOperation(intent, NoReopenKey())
	if err != nil {
		t.Fatal(err)
	}
	reference := lifecycleCheckpointReference(
		t, intent.OperationID(), intent.Digest(), intent.BindingDigest(), authority, seed+1,
	)
	evidence, err := AggregateDigestFromBytes(bytes.Repeat([]byte{seed + 2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	fixture := lifecycleCoverageFixture{
		operation: operation, reference: reference, evidence: evidence,
		receipts: make(map[DirectTreeReceiptKind]DirectTreeReceipt),
	}
	for _, kind := range []DirectTreeReceiptKind{
		ReceiptTreeCompletion, ReceiptPartialDirectory, ReceiptCleanup, ReceiptExpiry,
	} {
		fixture.receipts[kind] = lifecycleCoverageReceipt(t, fixture, kind)
	}
	return fixture
}

func lifecycleCoverageReceipt(
	t *testing.T,
	fixture lifecycleCoverageFixture,
	kind DirectTreeReceiptKind,
) DirectTreeReceipt {
	t.Helper()
	spec := DirectTreeReceiptSpec{
		Kind: kind, OperationID: fixture.operation.OperationID(),
		ReceiveIntent:     fixture.operation.ReceiveIntentDigest(),
		ReservationDigest: fixture.operation.BindingDigest(), EvidenceDigest: fixture.evidence,
	}
	switch kind {
	case ReceiptTreeCompletion:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.SuccessCount = 1
	case ReceiptPartialDirectory:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.SuccessCount, spec.FailureCount = 1, 1
		spec.PartialReason = PartialDirectoryFailures
	case ReceiptCleanup:
		spec.CleanupGeneration = 3
		spec.RemovedObjectCount, spec.RemovedRecordCount = 2, 4
	case ReceiptExpiry:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.SuccessCount, spec.FailureCount = 1, 1
		spec.CleanupGeneration = 5
		spec.RemovedObjectCount, spec.RemovedRecordCount = 2, 4
	}
	receipt, err := NewDirectTreeReceipt(spec)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func lifecycleCoverageState(
	t *testing.T,
	fixture lifecycleCoverageFixture,
	phase LifecyclePhase,
	generation uint64,
	cleanup OwnedCleanupState,
	reason NeedsAttentionReason,
) ReceiveLifecycleState {
	t.Helper()
	spec := LifecycleStateSpec{
		OperationID:     fixture.operation.OperationID(),
		ReceiveIntent:   fixture.operation.ReceiveIntentDigest(),
		StateGeneration: generation, Phase: phase,
	}
	switch phase {
	case LifecycleReceiving, LifecycleFinalizingTree:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.SuccessCount = 1
	case LifecycleResumableReceive:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.ExpiresAtMillis, spec.SuccessCount = 1_000, 1
	case LifecyclePublished:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.ReceiptDigest = fixture.receipts[ReceiptTreeCompletion].Digest()
		spec.SuccessCount, spec.CleanupState = 1, cleanup
	case LifecyclePartialDirectory:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.ReceiptDigest = fixture.receipts[ReceiptPartialDirectory].Digest()
		spec.SuccessCount, spec.FailureCount = 1, 1
		spec.PartialReason = PartialDirectoryFailures
	case LifecycleDiscarded:
		spec.ReceiptDigest = fixture.receipts[ReceiptCleanup].Digest()
		spec.CleanupState = OwnedCleanupClean
	case LifecycleExpired:
		spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
		spec.ReceiptDigest = fixture.receipts[ReceiptExpiry].Digest()
		spec.ExpiresAtMillis, spec.SuccessCount = 1_000, 1
		spec.FailureCount, spec.CleanupState = 1, cleanup
		spec.PriorStableState = LifecycleResumableReceive
	case LifecycleNeedsAttention:
		spec.AttentionReason = reason
	}
	state, err := NewReceiveLifecycleState(spec)
	if err != nil {
		t.Fatalf("phase %d generation %d: %v", phase, generation, err)
	}
	return state
}

func TestLifecycleFreshProcessDecodersRejectEveryTruncatedImage(t *testing.T) {
	fixture := newLifecycleCoverageFixture(t, 0x91)
	lifecycle := lifecycleCoverageState(
		t, fixture, LifecycleResumableReceive, 3, 0, 0,
	)
	lifecycleBytes, err := EncodeReceiveLifecycleState(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(lifecycleBytes) {
		if _, err := DecodeReceiveLifecycleState(lifecycleBytes[:cut]); !errors.Is(err, ErrInvalidLifecycleState) {
			t.Fatalf("lifecycle prefix %d/%d error = %v", cut, len(lifecycleBytes), err)
		}
	}

	receiptBytes := fixture.receipts[ReceiptExpiry].CanonicalBytes()
	for cut := range len(receiptBytes) {
		if _, err := DecodeDirectTreeReceipt(receiptBytes[:cut]); !errors.Is(err, ErrInvalidReceipt) {
			t.Fatalf("receipt prefix %d/%d error = %v", cut, len(receiptBytes), err)
		}
	}

	for name, test := range map[string]struct {
		encoded []byte
		decode  func([]byte) error
	}{
		"lifecycle oversized reference count": {
			encoded: func() []byte {
				encoded := bytes.Clone(lifecycleBytes)
				countOffset := len(encoded) - 4 - 32 - 8
				binary.BigEndian.PutUint32(encoded[countOffset:countOffset+4], MaximumAggregateReferences+1)
				return encoded
			}(),
			decode: func(encoded []byte) error {
				_, err := DecodeReceiveLifecycleState(encoded)
				return err
			},
		},
		"receipt oversized reference count": {
			encoded: func() []byte {
				encoded := bytes.Clone(receiptBytes)
				countOffset := len(encoded) - 4 - 32 - 8
				binary.BigEndian.PutUint32(encoded[countOffset:countOffset+4], MaximumAggregateReferences+1)
				return encoded
			}(),
			decode: func(encoded []byte) error {
				_, err := DecodeDirectTreeReceipt(encoded)
				return err
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := test.decode(test.encoded); err == nil {
				t.Fatal("tampered aggregate image decoded")
			}
		})
	}
}

func TestLifecycleTransitionMatrixKeepsCleanupAndOwnershipClosed(t *testing.T) {
	fixture := newLifecycleCoverageFixture(t, 0xa1)
	state := func(phase LifecyclePhase, generation uint64) ReceiveLifecycleState {
		return lifecycleCoverageState(t, fixture, phase, generation, 0, 0)
	}
	published := func(generation uint64, cleanup OwnedCleanupState) ReceiveLifecycleState {
		return lifecycleCoverageState(t, fixture, LifecyclePublished, generation, cleanup, 0)
	}
	expired := func(generation uint64, cleanup OwnedCleanupState) ReceiveLifecycleState {
		return lifecycleCoverageState(t, fixture, LifecycleExpired, generation, cleanup, 0)
	}
	attention := func(generation uint64, reason NeedsAttentionReason) ReceiveLifecycleState {
		return lifecycleCoverageState(t, fixture, LifecycleNeedsAttention, generation, 0, reason)
	}

	allowed := []struct {
		name     string
		previous ReceiveLifecycleState
		next     ReceiveLifecycleState
	}{
		{"frozen receiving", state(LifecycleIntentFrozen, 1), state(LifecycleReceiving, 2)},
		{"frozen discarded", state(LifecycleIntentFrozen, 1), state(LifecycleDiscarded, 2)},
		{"frozen attention", state(LifecycleIntentFrozen, 1), attention(2, AttentionTargetOwnershipUnknown)},
		{"receiving resumable", state(LifecycleReceiving, 1), state(LifecycleResumableReceive, 2)},
		{"receiving finalizing", state(LifecycleReceiving, 1), state(LifecycleFinalizingTree, 2)},
		{"receiving partial", state(LifecycleReceiving, 1), state(LifecyclePartialDirectory, 2)},
		{"receiving discarded", state(LifecycleReceiving, 1), state(LifecycleDiscarded, 2)},
		{"receiving attention", state(LifecycleReceiving, 1), attention(2, AttentionTargetOwnershipUnknown)},
		{"resumable receiving", state(LifecycleResumableReceive, 1), state(LifecycleReceiving, 2)},
		{"resumable finalizing", state(LifecycleResumableReceive, 1), state(LifecycleFinalizingTree, 2)},
		{"resumable partial", state(LifecycleResumableReceive, 1), state(LifecyclePartialDirectory, 2)},
		{"resumable discarded", state(LifecycleResumableReceive, 1), state(LifecycleDiscarded, 2)},
		{"resumable expired", state(LifecycleResumableReceive, 1), expired(2, OwnedCleanupPending)},
		{"resumable attention", state(LifecycleResumableReceive, 1), attention(2, AttentionTargetOwnershipUnknown)},
		{"finalizing published", state(LifecycleFinalizingTree, 1), published(2, OwnedCleanupPending)},
		{"finalizing resumable", state(LifecycleFinalizingTree, 1), state(LifecycleResumableReceive, 2)},
		{"finalizing partial", state(LifecycleFinalizingTree, 1), state(LifecyclePartialDirectory, 2)},
		{"finalizing discarded", state(LifecycleFinalizingTree, 1), state(LifecycleDiscarded, 2)},
		{"finalizing attention", state(LifecycleFinalizingTree, 1), attention(2, AttentionTargetOwnershipUnknown)},
		{"published cleanup", published(1, OwnedCleanupPending), published(2, OwnedCleanupClean)},
		{"published cleanup unknown", published(1, OwnedCleanupPending), attention(2, AttentionCleanupUnknown)},
		{"expired cleanup", expired(1, OwnedCleanupPending), expired(2, OwnedCleanupClean)},
		{"expired ownership unknown", expired(1, OwnedCleanupPending), attention(2, AttentionTargetOwnershipUnknown)},
		{"expired cleanup unknown", expired(1, OwnedCleanupPending), attention(2, AttentionCleanupUnknown)},
	}
	for _, test := range allowed {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateLifecycleTransition(test.previous, test.next); err != nil {
				t.Fatal(err)
			}
		})
	}

	foreign := newLifecycleCoverageFixture(t, 0xb1)
	invalid := []struct {
		name     string
		previous ReceiveLifecycleState
		next     ReceiveLifecycleState
	}{
		{"unsupported phase edge", state(LifecycleIntentFrozen, 1), published(2, OwnedCleanupPending)},
		{"generation gap", state(LifecycleIntentFrozen, 1), state(LifecycleReceiving, 3)},
		{"foreign operation", state(LifecycleIntentFrozen, 1), lifecycleCoverageState(t, foreign, LifecycleReceiving, 2, 0, 0)},
		{"published already clean", published(1, OwnedCleanupClean), published(2, OwnedCleanupClean)},
		{"generation exhausted", state(LifecycleIntentFrozen, math.MaxUint64), state(LifecycleReceiving, 1)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateLifecycleTransition(test.previous, test.next); !errors.Is(err, ErrInvalidLifecycleState) {
				t.Fatalf("transition error = %v", err)
			}
		})
	}
}

func TestDirectTreeReceiptVariantsRoundTripAggregateAuthority(t *testing.T) {
	fixture := newLifecycleCoverageFixture(t, 0xc1)
	for kind, receipt := range fixture.receipts {
		t.Run(string(rune('0'+kind)), func(t *testing.T) {
			restored, err := DecodeDirectTreeReceipt(receipt.CanonicalBytes())
			if err != nil || restored.Kind() != kind ||
				restored.OperationID() != fixture.operation.OperationID() ||
				restored.ReceiveIntentDigest() != fixture.operation.ReceiveIntentDigest() ||
				restored.ReservationDigest() != fixture.operation.BindingDigest() ||
				restored.EvidenceDigest() != fixture.evidence ||
				restored.SuccessCount() != receipt.SuccessCount() ||
				restored.FailureCount() != receipt.FailureCount() ||
				restored.PartialReason() != receipt.PartialReason() ||
				restored.CleanupGeneration() != receipt.CleanupGeneration() ||
				restored.RemovedObjectCount() != receipt.RemovedObjectCount() ||
				restored.RemovedRecordCount() != receipt.RemovedRecordCount() {
				t.Fatalf("receipt %d round trip = (%+v, %v)", kind, restored, err)
			}
		})
	}

	if _, err := AggregateDigestFromBytes(nil); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("short digest error = %v", err)
	}
	if _, err := AggregateDigestFromBytes(make([]byte, 32)); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("zero digest error = %v", err)
	}
	if _, err := FileCheckpointReferenceFromIdentity(RecordID{}, 1); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("zero reference error = %v", err)
	}
	if _, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: ReceiptCleanup, OperationID: fixture.operation.OperationID(),
		ReceiveIntent:     fixture.operation.ReceiveIntentDigest(),
		ReservationDigest: fixture.operation.BindingDigest(), EvidenceDigest: fixture.evidence,
	}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("generation-free cleanup receipt error = %v", err)
	}
	if _, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: ReceiptTreeCompletion, OperationID: fixture.operation.OperationID(),
		ReceiveIntent:     fixture.operation.ReceiveIntentDigest(),
		ReservationDigest: fixture.operation.BindingDigest(), EvidenceDigest: fixture.evidence,
		CheckpointRefs: []FileCheckpointReference{fixture.reference, fixture.reference},
	}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("duplicate aggregate reference error = %v", err)
	}
	if got := (NeedsAttentionReason(255)).String(); got != "" {
		t.Fatalf("open attention reason string = %q", got)
	}
	if fixture.reference.RecordID().IsZero() {
		t.Fatal("checkpoint reference lost its record identity")
	}
	partial := lifecycleCoverageState(t, fixture, LifecyclePartialDirectory, 2, 0, 0)
	published := lifecycleCoverageState(t, fixture, LifecyclePublished, 2, OwnedCleanupPending, 0)
	expired := lifecycleCoverageState(t, fixture, LifecycleExpired, 2, OwnedCleanupPending, 0)
	if partial.ReceiptDigest().IsZero() || partial.PartialReason() != PartialDirectoryFailures ||
		published.CleanupState() != OwnedCleanupPending ||
		expired.PriorStableState() != LifecycleResumableReceive {
		t.Fatal("terminal lifecycle accessors lost aggregate authority")
	}
	if (DirectTreeReceipt{}).CanonicalBytes() != nil || !(DirectTreeReceipt{}).Digest().IsZero() {
		t.Fatal("invalid receipt produced canonical authority")
	}
}

func TestLifecycleClosedUnionsRejectCrossPhaseFields(t *testing.T) {
	fixture := newLifecycleCoverageFixture(t, 0xd1)
	base := LifecycleStateSpec{
		OperationID:   fixture.operation.OperationID(),
		ReceiveIntent: fixture.operation.ReceiveIntentDigest(), StateGeneration: 1,
	}
	receipt := fixture.receipts[ReceiptTreeCompletion].Digest()
	invalidStates := map[string]LifecycleStateSpec{
		"frozen aggregate": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.SuccessCount = LifecycleIntentFrozen, 1
			return spec
		}(),
		"receiving expiry": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.ExpiresAtMillis = LifecycleReceiving, 1
			return spec
		}(),
		"resumable receipt": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.ExpiresAtMillis, spec.ReceiptDigest = LifecycleResumableReceive, 1, receipt
			return spec
		}(),
		"published failure": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.ReceiptDigest = LifecyclePublished, receipt
			spec.FailureCount, spec.CleanupState = 1, OwnedCleanupPending
			return spec
		}(),
		"partial without success": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.ReceiptDigest = LifecyclePartialDirectory, receipt
			spec.PartialReason = PartialDirectoryStopped
			return spec
		}(),
		"discarded checkpoint": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.ReceiptDigest = LifecycleDiscarded, fixture.receipts[ReceiptCleanup].Digest()
			spec.CleanupState = OwnedCleanupClean
			spec.CheckpointRefs = []FileCheckpointReference{fixture.reference}
			return spec
		}(),
		"expired prior phase": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.ReceiptDigest, spec.ExpiresAtMillis = LifecycleExpired, fixture.receipts[ReceiptExpiry].Digest(), 1
			spec.CleanupState, spec.PriorStableState = OwnedCleanupPending, LifecycleReceiving
			return spec
		}(),
		"attention expiry": func() LifecycleStateSpec {
			spec := base
			spec.Phase, spec.AttentionReason = LifecycleNeedsAttention, AttentionCleanupUnknown
			spec.ExpiresAtMillis = 1
			return spec
		}(),
		"unsupported direct tree phase": func() LifecycleStateSpec {
			spec := base
			spec.Phase = LifecyclePreparing
			return spec
		}(),
	}
	for name, spec := range invalidStates {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReceiveLifecycleState(spec); !errors.Is(err, ErrInvalidLifecycleState) {
				t.Fatalf("lifecycle error = %v", err)
			}
		})
	}
	if _, err := EncodeReceiveLifecycleState(ReceiveLifecycleState{}); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("zero lifecycle encode error = %v", err)
	}

	invalidReceipts := map[string]DirectTreeReceiptSpec{
		"tree failure": {
			Kind: ReceiptTreeCompletion, OperationID: fixture.operation.OperationID(),
			ReceiveIntent: fixture.operation.ReceiveIntentDigest(), ReservationDigest: fixture.operation.BindingDigest(),
			EvidenceDigest: fixture.evidence, FailureCount: 1,
		},
		"partial cleanup generation": {
			Kind: ReceiptPartialDirectory, OperationID: fixture.operation.OperationID(),
			ReceiveIntent: fixture.operation.ReceiveIntentDigest(), ReservationDigest: fixture.operation.BindingDigest(),
			EvidenceDigest: fixture.evidence, SuccessCount: 1, PartialReason: PartialDirectoryStopped,
			CleanupGeneration: 1,
		},
		"cleanup checkpoint": {
			Kind: ReceiptCleanup, OperationID: fixture.operation.OperationID(),
			ReceiveIntent: fixture.operation.ReceiveIntentDigest(), ReservationDigest: fixture.operation.BindingDigest(),
			EvidenceDigest: fixture.evidence, CleanupGeneration: 1,
			CheckpointRefs: []FileCheckpointReference{fixture.reference},
		},
		"expiry partial reason": {
			Kind: ReceiptExpiry, OperationID: fixture.operation.OperationID(),
			ReceiveIntent: fixture.operation.ReceiveIntentDigest(), ReservationDigest: fixture.operation.BindingDigest(),
			EvidenceDigest: fixture.evidence, CleanupGeneration: 1, PartialReason: PartialDirectoryStopped,
		},
		"open kind": {
			Kind: 255, OperationID: fixture.operation.OperationID(),
			ReceiveIntent: fixture.operation.ReceiveIntentDigest(), ReservationDigest: fixture.operation.BindingDigest(),
			EvidenceDigest: fixture.evidence,
		},
	}
	for name, spec := range invalidReceipts {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDirectTreeReceipt(spec); !errors.Is(err, ErrInvalidReceipt) {
				t.Fatalf("receipt error = %v", err)
			}
		})
	}

	sameGeneration, err := FileCheckpointReferenceFromIdentity(
		fixture.reference.RecordID(), fixture.reference.CheckpointGeneration()+1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: ReceiptTreeCompletion, OperationID: fixture.operation.OperationID(),
		ReceiveIntent: fixture.operation.ReceiveIntentDigest(), ReservationDigest: fixture.operation.BindingDigest(),
		EvidenceDigest: fixture.evidence,
		CheckpointRefs: []FileCheckpointReference{sameGeneration, fixture.reference},
	}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("dual checkpoint generation error = %v", err)
	}
}

func TestReceiveOperationFreshProcessDecoderRejectsEveryTruncatedImage(t *testing.T) {
	intent, authority := receiveOperationIntentFixture(t, 0xe1)
	key, err := NewCLICompatibleOperationKey(intent.SelectionSpec(), intent.ArtifactSpec(), authority)
	if err != nil {
		t.Fatal(err)
	}
	reopen, err := CLIReopenKey(key)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := NewReceiveOperation(intent, reopen)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeReceiveOperation(operation)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(encoded) {
		if _, err := DecodeReceiveOperation(encoded[:cut]); !errors.Is(err, ErrInvalidReceiveOperation) {
			t.Fatalf("operation prefix %d/%d error = %v", cut, len(encoded), err)
		}
	}
	if _, err := operation.VerifyIntent(nil); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("nil intent decoder error = %v", err)
	}
	decoderErr := errors.New("decode failed")
	if _, err := operation.VerifyIntent(func([]byte) (transfer.ReceiveIntent, error) {
		return transfer.ReceiveIntent{}, decoderErr
	}); !errors.Is(err, ErrInvalidReceiveOperation) || !errors.Is(err, decoderErr) {
		t.Fatalf("failed intent decoder error = %v", err)
	}
	if _, err := CompatibleOperationKeyFromBytes(nil); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("short compatible key error = %v", err)
	}
	if _, err := NewCLICompatibleOperationKey(transfer.SelectionSpec{}, intent.ArtifactSpec(), authority); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("zero selection compatible key error = %v", err)
	}
	if _, err := NewReceiveOperation(transfer.ReceiveIntent{}, NoReopenKey()); !errors.Is(err, ErrInvalidReceiveOperation) {
		t.Fatalf("zero receive operation error = %v", err)
	}
}

func TestAdmittedDirectoryFreshProcessCodecRejectsEveryTruncatedField(t *testing.T) {
	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	intent, admission, owned, _ := admittedDirectoryFixture(t, 0xf1, modified)
	record, err := NewAdmittedDirectory(intent, admission, owned)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeAdmittedDirectory(record)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(encoded) {
		if _, err := DecodeAdmittedDirectory(encoded[:cut]); !errors.Is(err, ErrInvalidAdmittedDirectory) {
			t.Fatalf("admitted-directory prefix %d/%d error = %v", cut, len(encoded), err)
		}
	}
	restored, err := DecodeAdmittedDirectory(encoded)
	if err != nil || restored.LayoutVersion() != transfer.DirectoryAdmissionLayoutV1 ||
		restored.Layout() != admission.Layout() || restored.ModifiedTime() != modified {
		t.Fatalf("fresh admitted-directory decode = (%+v, %v)", restored, err)
	}

	for name, encodedTime := range map[string][]byte{
		"empty":          nil,
		"open marker":    {2},
		"short present":  {1, 0},
		"open precision": append(bytes.Repeat([]byte{0}, 13), 0xff),
		"invalid nanos": func() []byte {
			value := make([]byte, 14)
			value[0] = 1
			binary.BigEndian.PutUint32(value[9:13], 1_000_000_000)
			value[13] = byte(catalog.TimePrecisionNanoseconds)
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeModifiedTime(encodedTime); !errors.Is(err, ErrInvalidAdmittedDirectory) {
				t.Fatalf("modified-time error = %v", err)
			}
		})
	}
	if got := encodeModifiedTime(catalog.ModifiedTime{}); !bytes.Equal(got, []byte{0}) {
		t.Fatalf("absent modified time = %x", got)
	}
	if _, err := EncodeAdmittedDirectory(AdmittedDirectory{}); !errors.Is(err, ErrInvalidAdmittedDirectory) {
		t.Fatalf("zero admitted-directory encode error = %v", err)
	}
}

func TestCheckpointAuthorityCodecsRejectEveryFreshProcessPrefix(t *testing.T) {
	record := mustCanonicalRecord(t, canonicalRecordSpec(t))
	recordImage, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(recordImage) {
		if _, err := DecodeRecord(recordImage[:cut]); err == nil {
			t.Fatalf("record prefix %d/%d decoded", cut, len(recordImage))
		}
	}
	recordPayload := record.CanonicalBytes()
	for cut := range len(recordPayload) {
		if _, err := DecodeRecord(recordEnvelope(recordPayload[:cut])); err == nil {
			t.Fatalf("authenticated record payload prefix %d/%d decoded", cut, len(recordPayload))
		}
	}

	ownership, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerNativeTree, Certification: CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        bytes.Repeat([]byte{0xf2}, 32),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownershipImage, err := EncodeOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(ownershipImage) {
		if _, err := DecodeOwnership(ownershipImage[:cut]); err == nil {
			t.Fatalf("ownership prefix %d/%d decoded", cut, len(ownershipImage))
		}
	}
	ownershipPayload := ownership.CanonicalBytes()
	for cut := range len(ownershipPayload) {
		prefix := bytes.Clone(ownershipPayload[:cut])
		checksum := ownershipChecksum(prefix)
		prefix = append(prefix, checksum[:]...)
		if _, err := DecodeOwnership(prefix); err == nil {
			t.Fatalf("authenticated ownership payload prefix %d/%d decoded", cut, len(ownershipPayload))
		}
	}
}

func TestRecordValidationRejectsInMemoryAuthorityTampering(t *testing.T) {
	canonical := mustCanonicalRecord(t, canonicalRecordSpec(t))
	mutations := map[string]func(*Record){
		"ownership marker": func(record *Record) { record.ownershipMarker = "foreign" },
		"namespace":        func(record *Record) { record.namespace = "foreign" },
		"identity":         func(record *Record) { record.stateGeneration = 0 },
		"canonical path":   func(record *Record) { record.canonicalPath = "folder/../file.bin" },
		"phase":            func(record *Record) { record.phase = 0 },
		"commit state":     func(record *Record) { record.commitState = 0 },
		"phase commit": func(record *Record) {
			record.phase, record.commitState = PhasePublished, CommitCandidate
		},
		"lifecycle claim": func(record *Record) {
			record.quarantineReason = QuarantineAnchorMissing
		},
		"verified ranges": func(record *Record) {
			record.verifiedRanges = []Range{{Offset: 16, End: 16}}
		},
		"record identity": func(record *Record) { record.recordID[0] ^= 0xff },
		"checksum":        func(record *Record) { record.checksum[0] ^= 0xff },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			tampered := canonical
			tampered.verifiedRanges = append([]Range(nil), canonical.verifiedRanges...)
			mutate(&tampered)
			if tampered.Valid() || tampered.CanonicalBytes() != nil {
				t.Fatal("tampered checkpoint remained canonical")
			}
			if _, err := EncodeRecord(tampered); err == nil {
				t.Fatal("tampered checkpoint acquired persisted authority")
			}
		})
	}
}

func TestQuarantineHistoryAllowsOnlyReachableRuntimeOrigins(t *testing.T) {
	tests := []struct {
		name   string
		origin QuarantineOrigin
		reason QuarantineReason
		want   bool
	}{
		{"anchor missing after witness", QuarantineOriginWitnessed, QuarantineAnchorMissing, true},
		{"anchor missing before witness", QuarantineOriginReserved, QuarantineAnchorMissing, false},
		{"stage missing while publishing", QuarantineOriginPublishing, QuarantineStageMissing, true},
		{"stage missing after publish", QuarantineOriginPublished, QuarantineStageMissing, false},
		{"final mismatch after publish", QuarantineOriginPublished, QuarantineFinalMismatch, true},
		{"final mismatch before publish", QuarantineOriginPublishing, QuarantineFinalMismatch, false},
		{"final unsafe from active runtime", QuarantineOriginPublishBlocked, QuarantineFinalUnsafe, true},
		{"final unsafe while retiring", QuarantineOriginRetiring, QuarantineFinalUnsafe, false},
		{"partial object at reservation", QuarantineOriginReserved, QuarantinePartialObjectCreation, true},
		{"partial object while retiring", QuarantineOriginRetiring, QuarantinePartialObjectCreation, true},
		{"partial object after witness", QuarantineOriginWitnessed, QuarantinePartialObjectCreation, false},
		{"publication history while blocked", QuarantineOriginPublishBlocked, QuarantinePublicationHistory, true},
		{"publication history after publish", QuarantineOriginPublished, QuarantinePublicationHistory, false},
		{"metadata mismatch while publishing", QuarantineOriginPublishing, QuarantineMetadataMismatch, true},
		{"metadata mismatch at reservation", QuarantineOriginReserved, QuarantineMetadataMismatch, false},
		{"unsafe stage at any valid origin", QuarantineOriginRetiring, QuarantineStageUnsafe, true},
		{"unknown reason", QuarantineOriginReserved, 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validQuarantineHistory(test.origin, test.reason); got != test.want {
				t.Fatalf("history validity = %t, want %t", got, test.want)
			}
		})
	}

	spec := canonicalRecordSpec(t)
	spec.Phase, spec.CommitState = PhaseQuarantined, CommitQuarantined
	spec.QuarantineReason, spec.QuarantineOrigin = QuarantineMetadataMismatch, QuarantineOriginPublishing
	quarantined := mustCanonicalRecord(t, spec)
	if quarantined.FileID().IsZero() || quarantined.FileRevision().IsZero() ||
		quarantined.QuarantineReason() != QuarantineMetadataMismatch ||
		quarantined.QuarantineOrigin() != QuarantineOriginPublishing || quarantined.RetirementReason() != 0 {
		t.Fatal("quarantine authority projection lost immutable evidence")
	}
}

func TestCheckpointReducerRejectsEveryNonAdjacentAuthorityCut(t *testing.T) {
	candidate := mustCanonicalRecord(t, canonicalRecordSpec(t))
	committed, err := Promote(candidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}

	invalidPrevious := committed
	invalidPrevious.stateGeneration = 0
	if err := ValidateTransition(invalidPrevious, committed); err == nil {
		t.Fatal("invalid previous record entered the reducer")
	}
	invalidNext := committed
	invalidNext.checksum[0] ^= 0xff
	if err := ValidateTransition(committed, invalidNext); err == nil {
		t.Fatal("invalid next record entered the reducer")
	}

	pausedSpec := canonicalRecordSpec(t)
	pausedSpec.StateGeneration = committed.StateGeneration() + 1
	pausedSpec.Phase, pausedSpec.CommitState = PhasePaused, CommitVerified
	paused := mustCanonicalRecord(t, pausedSpec)
	if err := ValidateTransition(paused, committed); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("generation regression error = %v", err)
	}
	if err := ValidateTransition(candidate, paused); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("candidate phase substitution error = %v", err)
	}

	activeSuccessorSpec := canonicalRecordSpec(t)
	activeSuccessorSpec.StateGeneration = committed.StateGeneration() + 1
	activeSuccessorSpec.CommitState = CommitVerified
	activeSuccessor := mustCanonicalRecord(t, activeSuccessorSpec)
	if err := ValidateTransition(committed, activeSuccessor); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("unsupported same-phase transition error = %v", err)
	}

	gapSpec := canonicalRecordSpec(t)
	gapSpec.StateGeneration = committed.StateGeneration() + 1
	gapSpec.CheckpointGeneration = committed.CheckpointGeneration() + 2
	gap := mustCanonicalRecord(t, gapSpec)
	if err := ValidateTransition(committed, gap); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("checkpoint generation gap error = %v", err)
	}

	regressedSpec := pausedSpec
	regressedSpec.VerifiedRanges = []Range{{Offset: 0, End: 16}}
	regressed := mustCanonicalRecord(t, regressedSpec)
	if err := ValidateTransition(committed, regressed); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("verified range regression error = %v", err)
	}
}

func TestCheckpointReducerErrorPathsPreserveLastCommittedEvidence(t *testing.T) {
	candidate := mustCanonicalRecord(t, canonicalRecordSpec(t))
	committed, err := Promote(candidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}

	if ValidLifecycleTransition(PhasePublishing, CommitVerified, PhasePublished, CommitVerified) ||
		ValidLifecycleTransition(PhaseActive, CommitVerified, PhaseQuarantined, CommitVerified) ||
		ValidLifecycleTransition(PhaseActive, CommitVerified, PhaseActive, CommitVerified) ||
		ValidLifecycleTransition(PhasePublished, CommitPublished, PhaseRetired, CommitPublished) {
		t.Fatal("open lifecycle edge acquired mutation authority")
	}
	if rangesContain([]Range{{Offset: 32, End: 64}}, []Range{{Offset: 0, End: 16}}) {
		t.Fatal("later range was treated as containing earlier durable bytes")
	}
	if _, err := SelectVerified(checkpointmodelInvalidRecordForCoverage(committed)); err == nil {
		t.Fatal("invalid checkpoint was selected as verified")
	}
	if _, err := Recover(&Record{}, nil); err == nil {
		t.Fatal("invalid committed checkpoint recovered")
	}
	recovered, err := Recover(&committed, nil)
	if err != nil || !IdentityEqual(recovered, committed) {
		t.Fatalf("nil candidate recovery = (%+v, %v)", recovered, err)
	}

	if _, err := AdvanceGeneration(Record{}, nil, PhaseActive, CommitCandidate); err == nil {
		t.Fatal("invalid generation source advanced")
	}
	overflowSpec := canonicalRecordSpec(t)
	overflowSpec.StateGeneration, overflowSpec.CheckpointGeneration = math.MaxUint64, math.MaxUint64
	overflowSpec.CommitState = CommitVerified
	overflow := mustCanonicalRecord(t, overflowSpec)
	if _, err := AdvanceGeneration(overflow, overflow.VerifiedRanges(), PhaseActive, CommitCandidate); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("generation overflow error = %v", err)
	}
	if _, err := AdvanceGeneration(committed, []Range{{Offset: 16, End: 16}}, PhaseActive, CommitCandidate); err == nil {
		t.Fatal("invalid next-generation ranges were admitted")
	}
	if _, err := AdvanceGeneration(committed, []Range{{Offset: 0, End: 16}}, PhaseActive, CommitCandidate); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("regressed next-generation ranges error = %v", err)
	}

	if _, err := AdvanceState(committed, committed.StateGeneration()+1, 0, CommitVerified, 0, 0, 0); err == nil {
		t.Fatal("invalid lifecycle state was constructed")
	}
	if _, err := AdvanceState(
		committed, committed.StateGeneration()+1, PhaseActive, CommitVerified, 0, 0, 0,
	); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("unsupported state transition error = %v", err)
	}
	if _, err := Promote(Record{}, PhaseActive, CommitVerified); err == nil {
		t.Fatal("invalid candidate was promoted")
	}
	if _, err := Promote(candidate, PhaseQuarantined, CommitVerified); err == nil {
		t.Fatal("invalid promoted phase was constructed")
	}
	if _, err := Promote(candidate, PhasePaused, CommitVerified); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("candidate phase substitution promotion error = %v", err)
	}
}

func checkpointmodelInvalidRecordForCoverage(record Record) Record {
	record.checksum[0] ^= 0xff
	return record
}

func TestRangeCanonicalizationAndChecksumViewsRemainBounded(t *testing.T) {
	if got := (Range{Offset: 4, End: 4}).Length(); got != 0 {
		t.Fatalf("empty range length = %d", got)
	}
	if got := (Range{Offset: 4, End: 9}).Length(); got != 5 {
		t.Fatalf("range length = %d", got)
	}
	if err := validateRanges(make([]Range, maximumRanges+1), catalog.MaxFileSize); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("range-count bound error = %v", err)
	}

	for name, test := range map[string]struct {
		ranges []Range
		want   Range
	}{
		"shorter duplicate first": {[]Range{{Offset: 0, End: 2}, {Offset: 0, End: 3}}, Range{Offset: 0, End: 3}},
		"longer duplicate first":  {[]Range{{Offset: 0, End: 3}, {Offset: 0, End: 2}}, Range{Offset: 0, End: 3}},
		"exact duplicate":         {[]Range{{Offset: 0, End: 2}, {Offset: 0, End: 2}}, Range{Offset: 0, End: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, err := CanonicalizeRanges(test.ranges)
			if err != nil || len(canonical) != 1 || canonical[0] != test.want {
				t.Fatalf("canonical ranges = (%+v, %v)", canonical, err)
			}
		})
	}

	record := mustCanonicalRecord(t, canonicalRecordSpec(t))
	checksum := record.Checksum().Bytes()
	checksum[0] ^= 0xff
	if bytes.Equal(checksum, record.Checksum().Bytes()) {
		t.Fatal("checksum byte view aliases persisted checkpoint authority")
	}
}
