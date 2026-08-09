package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
)

func TestOutputSettlementSumTypesRejectMalformedStates(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)

	if _, err := NewVerifiedFileSettlement(FilePublished, VerifiedDurableRanges{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("published settlement without a binding error = %v", err)
	}
	if _, err := NewMaterializationStateRef(OutputSessionID{}, binding.Locator().Digest()); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("state reference without a session error = %v", err)
	}
	if _, err := NewMaterializationStateRef(binding.OutputSessionID(), MaterializationLocatorDigest{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("state reference without a locator error = %v", err)
	}

	reference, err := NewMaterializationStateRef(binding.OutputSessionID(), binding.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewImmediateQuarantinedFileSettlement(
		FileMaterializationTarget{}, reference, QuarantineOwnershipMismatch,
	); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unbound immediate quarantine error = %v", err)
	}
	var foreignLocator MaterializationLocatorDigest
	foreignLocator[0] = 99
	foreignReference, err := NewMaterializationStateRef(binding.OutputSessionID(), foreignLocator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTransactionQuarantinedFileSettlement(
		binding, foreignReference, QuarantineOwnershipMismatch,
	); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("foreign transaction quarantine error = %v", err)
	}

	if FileSettlementKind(0).valid() || !FilePublished.valid() || (FileQuarantined + 1).valid() {
		t.Fatal("file settlement kind admitted a value outside its closed domain")
	}
	malformedQuarantine := FileSettlement{kind: FileQuarantined, target: binding.Target()}
	if malformedQuarantine.valid() {
		t.Fatal("quarantine without durable state evidence was valid")
	}
	if (FileSettlement{kind: FileSettlementKind(255)}).valid() {
		t.Fatal("unknown settlement kind was valid")
	}
	if (FileSettlement{}).matchesBinding(binding) {
		t.Fatal("zero settlement matched an owned binding")
	}

	if _, err := NewFileTransactionStart(nil, checkpoint); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("nil transaction start error = %v", err)
	}
	if _, err := NewFileSettlementStart(FileSettlement{kind: FilePublished}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("malformed immediate publication error = %v", err)
	}
	if (FileStart{}).valid() {
		t.Fatal("zero file start was valid")
	}
}

func TestOutputLifecycleReasonsAndAuthorityFunctionAreClosedDomains(t *testing.T) {
	if FilePauseReason(0).valid() || !FilePauseInterrupted.valid() || (FilePauseDependencyContract + 1).valid() {
		t.Fatal("file pause reason admitted a value outside its closed domain")
	}
	if JobPauseReason(0).valid() || !JobPauseInterrupted.valid() || (JobPauseDependencyContract + 1).valid() {
		t.Fatal("job pause reason admitted a value outside its closed domain")
	}
	if _, err := NewDirectTreeSettlement(DirectTreeSettlementKind(0)); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unknown job settlement error = %v", err)
	}

	var nilAuthority DirectTreeMaterializerFunc
	if _, err := nilAuthority.OpenDirectTree(context.Background(), ReceiveIntent{}); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("nil output authority error = %v", err)
	}
	want := errors.New("authority invoked")
	called := false
	authority := DirectTreeMaterializerFunc(func(context.Context, ReceiveIntent) (DirectTreeSession, error) {
		called = true
		return nil, want
	})
	if _, err := authority.OpenDirectTree(context.Background(), ReceiveIntent{}); !called || !errors.Is(err, want) {
		t.Fatalf("authority call = (%v, %v), want delegated error", called, err)
	}
}

func outputLifecycleFixture(t *testing.T) (MaterializedFileBinding, VerifiedDurableRanges) {
	t.Helper()
	descriptor := transferDescriptor(t, 1)
	locator, err := NewPathMaterializationLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var objectIdentity OwnedObjectID
	objectIdentity[0] = 32
	binding, err := NewMaterializedFileBinding(
		transferID[OutputSessionID](31),
		descriptor,
		locator,
		objectIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize()}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := VerifyDurableRanges(binding, 1, ranges)
	if err != nil {
		t.Fatal(err)
	}
	return binding, checkpoint
}
