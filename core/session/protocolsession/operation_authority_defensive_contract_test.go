package protocolsession

import (
	"errors"
	"testing"
)

func TestOperationAuthorityQueriesRemainGenerationScoped(t *testing.T) {
	var zero OperationGeneration
	if zero.IsCurrent() || zero.IsActive() {
		t.Fatal("zero generation reported current or active authority")
	}
	if maximum, ok := zero.MaximumContinuations(); ok || maximum != 0 {
		t.Fatalf("zero generation continuation limit = (%d, %t)", maximum, ok)
	}
	if kind, ok := zero.RequestKind(); ok || kind != 0 {
		t.Fatalf("zero generation request kind = (%v, %t)", kind, ok)
	}

	table, admission, _ := operationAuthorityAdmission(t, OperationLimits{MaxActive: 2, MaxTracked: 2})
	if maximum, ok := admission.Generation.MaximumContinuations(); ok || maximum != 0 {
		t.Fatalf("generation without continuation authority = (%d, %t)", maximum, ok)
	}
	admission.Generation.authority.continuations = &operationContinuationState{maximum: 7}
	if maximum, ok := admission.Generation.MaximumContinuations(); !ok || maximum != 7 {
		t.Fatalf("generation continuation limit = (%d, %t), want (7, true)", maximum, ok)
	}
	admission.Generation.authority.continuations = nil
	if kind, ok := admission.Generation.RequestKind(); !ok || kind != MessageRequestBlocks {
		t.Fatalf("active generation request kind = (%v, %t)", kind, ok)
	}
	if err := table.CancelGeneration(admission.Generation); err != nil {
		t.Fatal(err)
	}
	if kind, ok := admission.Generation.RequestKind(); !ok || kind != MessageRequestBlocks {
		t.Fatalf("tombstoned generation request kind = (%v, %t)", kind, ok)
	}

	stale := admission.Generation
	stale.authority = &operationAuthority{}
	if kind, ok := stale.RequestKind(); ok || kind != 0 {
		t.Fatalf("stale generation request kind = (%v, %t)", kind, ok)
	}
}

func TestCancelGenerationRejectsTerminalStaleAndPreservesReservedCapacity(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		table, admission, _ := operationAuthorityAdmission(t, OperationLimits{MaxActive: 2, MaxTracked: 2})
		table.terminal = true
		if err := table.CancelGeneration(admission.Generation); !errors.Is(err, ErrSessionTerminated) {
			t.Fatalf("terminal cancellation error = %v", err)
		}
	})

	t.Run("different tombstone generation", func(t *testing.T) {
		table, admission, operationID := operationAuthorityAdmission(t, OperationLimits{MaxActive: 2, MaxTracked: 2})
		table.tombstones[operationID] = operationTombstone{
			expiresAt: table.now().Add(OperationTombstoneLifetime), authority: &operationAuthority{},
		}
		if err := table.CancelGeneration(admission.Generation); err != nil {
			t.Fatalf("stale tombstone cancellation error = %v", err)
		}
	})

	t.Run("missing active generation", func(t *testing.T) {
		table, admission, operationID := operationAuthorityAdmission(t, OperationLimits{MaxActive: 2, MaxTracked: 2})
		delete(table.active, operationID)
		if err := table.CancelGeneration(admission.Generation); err != nil {
			t.Fatalf("missing generation cancellation error = %v", err)
		}
	})

	t.Run("reserved cancellation capacity", func(t *testing.T) {
		table, admission, operationID := operationAuthorityAdmission(t, OperationLimits{MaxActive: 2, MaxTracked: 1})
		otherID := testOperationID(0xe2)
		if otherID == operationID {
			t.Fatal("test operation identities collided")
		}
		cancel := mustMessage(t, MessageCancel, &otherID, map[uint64]any{0: uint64(1)})
		if _, err := table.Observe(DirectionReceiverToSender, cancel); !errors.Is(err, ErrTrackedOperationBudget) {
			t.Fatalf("unknown cancel stole an admitted operation's slot: %v", err)
		}
		if err := table.CancelGeneration(admission.Generation); err != nil {
			t.Fatalf("reserved cancellation error = %v", err)
		}
	})
}

func TestOutboundPermitRejectsZeroStaleAndOverBudgetAuthority(t *testing.T) {
	var zero OutboundOperationPermit
	if generation := zero.Generation(); !generation.IsZero() {
		t.Fatal("zero permit minted a generation")
	}
	if lease, err := zero.AcquireLease(); lease != nil || !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("zero permit lease = (%T, %v)", lease, err)
	}

	_, admission, _ := operationAuthorityAdmission(t, OperationLimits{MaxActive: 2, MaxTracked: 2})
	stale := admission.Outbound
	stale.authority = &operationAuthority{}
	if lease, err := stale.AcquireLease(); lease != nil || !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("stale permit lease = (%T, %v)", lease, err)
	}

	admission.Outbound.authority.pins = MaximumOperationPins
	if lease, err := admission.Outbound.AcquireLease(); lease != nil || !errors.Is(err, ErrOperationPinBudget) {
		t.Fatalf("pin-budget lease = (%T, %v)", lease, err)
	}

	var nilLease *OutboundOperationLease
	nilLease.Release()
}

func operationAuthorityAdmission(
	t *testing.T,
	limits OperationLimits,
) (*OperationTable, InboundAdmission, OperationID) {
	t.Helper()
	table, err := NewOperationTable(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(0xe1)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	admission, err := table.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil || admission.Disposition != OperationDeliver || admission.Generation.IsZero() || admission.Outbound.IsZero() {
		t.Fatalf("inbound admission = (%+v, %v)", admission, err)
	}
	return table, admission, operationID
}
