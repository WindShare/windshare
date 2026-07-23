package protocolsession

import (
	"errors"
	"testing"
)

func TestOperationTombstoneDropsOnlyRequestCompatibleLateContinuations(t *testing.T) {
	tests := []struct {
		name         string
		request      MessageKind
		final        MessageKind
		late         MessageKind
		lateFragment bool
	}{
		{name: "catalog progress", request: MessageListChildren, final: MessageCatalogResult, late: MessageScanProgress},
		{name: "peer candidate", request: MessagePeerOffer, final: MessageOperationError, late: MessagePeerCandidate},
		{name: "block fragment", request: MessageRequestBlocks, final: MessageOperationComplete, late: MessageBlockFragment, lateFragment: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, err := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
			if err != nil {
				t.Fatal(err)
			}
			operationID := testOperationID(byte(190 + index))
			request := mustMessage(t, test.request, &operationID, map[uint64]any{0: uint64(1)})
			if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDeliver {
				t.Fatalf("request: disposition=%d error=%v", disposition, err)
			}
			final := mustMessage(t, test.final, &operationID, map[uint64]any{0: uint64(1)})
			if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDeliver {
				t.Fatalf("final: disposition=%d error=%v", disposition, err)
			}
			if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDrop {
				t.Fatalf("exact request replay: disposition=%d error=%v", disposition, err)
			}
			conflictingRequest := mustMessage(t, test.request, &operationID, map[uint64]any{0: uint64(2)})
			if _, err := table.Observe(DirectionReceiverToSender, conflictingRequest); !errors.Is(err, ErrOperationIDReused) {
				t.Fatalf("conflicting request replay error=%v", err)
			}
			var late Message
			if test.lateFragment {
				late = mustFragmentMessage(t, operationID, 1)
			} else {
				late = mustMessage(t, test.late, &operationID, map[uint64]any{0: uint64(2)})
			}
			if disposition, err := table.Observe(DirectionSenderToReceiver, late); err != nil || disposition != OperationDrop {
				t.Fatalf("compatible late continuation: disposition=%d error=%v", disposition, err)
			}
		})
	}
}

func TestCatalogTombstoneRejectsUnrelatedLateBlockFragment(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(199)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	final := mustMessage(t, MessageCatalogResult, &operationID, map[uint64]any{0: uint64(1)})
	_, _ = table.Observe(DirectionReceiverToSender, request)
	_, _ = table.Observe(DirectionSenderToReceiver, final)
	if _, err := table.Observe(DirectionSenderToReceiver, mustFragmentMessage(t, operationID, 1)); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("unrelated late fragment error=%v", err)
	}
}

func TestCancelledTombstoneScopesLateTrafficAndFingerprintsRacedFinal(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(200)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	cancel := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDeliver {
		t.Fatalf("request: disposition=%d error=%v", disposition, err)
	}
	if disposition, err := table.Observe(DirectionReceiverToSender, cancel); err != nil || disposition != OperationDeliver {
		t.Fatalf("cancel: disposition=%d error=%v", disposition, err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, mustFragmentMessage(t, operationID, 1)); err != nil || disposition != OperationDrop {
		t.Fatalf("compatible fragment: disposition=%d error=%v", disposition, err)
	}
	final := mustMessage(t, MessageOperationComplete, &operationID, map[uint64]any{0: uint64(1)})
	for attempt := 0; attempt < 2; attempt++ {
		if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDrop {
			t.Fatalf("exact raced final %d: disposition=%d error=%v", attempt, disposition, err)
		}
	}
	conflicting := mustMessage(t, MessageOperationComplete, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.Observe(DirectionSenderToReceiver, conflicting); !errors.Is(err, ErrConflictingFinal) {
		t.Fatalf("conflicting raced final error=%v", err)
	}
	unrelated := mustMessage(t, MessageCatalogResult, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionSenderToReceiver, unrelated); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("cross-kind traffic error=%v", err)
	}
}

func TestPreemptiveCancelLearnsOnlyTheRacedRequestFamily(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(201)
	cancel := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionReceiverToSender, cancel); err != nil || disposition != OperationDeliver {
		t.Fatalf("preemptive cancel: disposition=%d error=%v", disposition, err)
	}
	if disposition, err := table.Observe(DirectionReceiverToSender, cancel); err != nil || disposition != OperationDrop {
		t.Fatalf("repeated cancel: disposition=%d error=%v", disposition, err)
	}
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDrop {
		t.Fatalf("raced request: disposition=%d error=%v", disposition, err)
	}
	if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDrop {
		t.Fatalf("exact raced request replay: disposition=%d error=%v", disposition, err)
	}
	conflictingRequest := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.Observe(DirectionReceiverToSender, conflictingRequest); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("conflicting raced request replay error=%v", err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, mustFragmentMessage(t, operationID, 1)); err != nil || disposition != OperationDrop {
		t.Fatalf("compatible continuation: disposition=%d error=%v", disposition, err)
	}
	unrelated := mustMessage(t, MessageScanProgress, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionSenderToReceiver, unrelated); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("unrelated response error=%v", err)
	}
}

func TestPreemptiveCancelRejectsUnsolicitedResponseBeforeRequest(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(202)
	cancel := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
	_, _ = table.Observe(DirectionReceiverToSender, cancel)
	unsolicited := mustMessage(t, MessageCatalogResult, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionSenderToReceiver, unsolicited); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("unsolicited response error=%v", err)
	}
}
