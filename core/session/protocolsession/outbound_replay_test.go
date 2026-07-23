package protocolsession

import (
	"errors"
	"testing"
	"time"
)

func TestOutboundReplayPermitBindsAuthorityAndCanonicalMessage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	table, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, func() time.Time { return now })
	operationID := testOperationID(101)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	authority := observeSenderOperation(t, table, request)
	fragment := mustFragmentMessage(t, operationID, 1)
	fragmentAdmission, err := table.AdmitOutbound(DirectionSenderToReceiver, fragment, authority)
	if err != nil || fragmentAdmission.Disposition != OperationDeliver || fragmentAdmission.Replay.IsZero() {
		t.Fatalf("fragment admission=%+v error=%v", fragmentAdmission, err)
	}
	fragmentReplay, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, fragment, fragmentAdmission.Replay,
	)
	if err != nil || fragmentReplay.Disposition != OperationDeliver {
		t.Fatalf("fragment replay=%v error=%v", fragmentReplay.Disposition, err)
	}
	fragmentReplay.pin.release()
	fragmentAdmission.pin.release()
	otherFragment := mustFragmentMessage(t, operationID, 2)
	if _, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, otherFragment, fragmentAdmission.Replay,
	); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("mismatched fragment replay error=%v", err)
	}

	final := mustMessage(t, MessageOperationComplete, &operationID, map[uint64]any{0: uint64(1)})
	finalAdmission, err := table.AdmitOutbound(DirectionSenderToReceiver, final, authority)
	if err != nil || finalAdmission.Disposition != OperationDeliver || finalAdmission.Replay.IsZero() {
		t.Fatalf("final admission=%+v error=%v", finalAdmission, err)
	}
	if fragmentAdmission.Replay.authority != finalAdmission.Replay.authority {
		t.Fatal("one operation changed replay authority at finalization")
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDrop {
		t.Fatalf("normal duplicate final=%v error=%v", disposition, err)
	}
	finalReplay, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, final, finalAdmission.Replay,
	)
	if err != nil || finalReplay.Disposition != OperationDeliver {
		t.Fatalf("permitted final replay=%v error=%v", finalReplay.Disposition, err)
	}
	finalReplay.pin.release()
	originalDeadline := now.Add(OperationTombstoneLifetime)
	for replayIndex := range 2 {
		now = now.Add(OperationTombstoneLifetime / 3)
		delayedReplay, replayErr := table.AcceptOutboundReplay(
			DirectionSenderToReceiver, final, finalAdmission.Replay,
		)
		if replayErr != nil || delayedReplay.Disposition != OperationDeliver {
			t.Fatalf("delayed final replay %d=%v error=%v", replayIndex, delayedReplay.Disposition, replayErr)
		}
		delayedReplay.pin.release()
	}
	conflictingFinal := mustMessage(t, MessageOperationComplete, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, conflictingFinal, finalAdmission.Replay,
	); !errors.Is(err, ErrConflictingFinal) {
		t.Fatalf("conflicting final replay error=%v", err)
	}
	finalAdmission.pin.release()
	now = originalDeadline.Add(time.Nanosecond)
	if stale, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, final, finalAdmission.Replay,
	); err != nil || stale.Disposition != OperationDrop {
		t.Fatalf("expired final replay=%v error=%v", stale.Disposition, err)
	}
	newAuthority := observeSenderOperation(t, table, request)
	newAdmission, err := table.AdmitOutbound(DirectionSenderToReceiver, fragment, newAuthority)
	if err != nil || newAdmission.Replay.IsZero() {
		t.Fatalf("same ID new generation admission=%+v error=%v", newAdmission, err)
	}
	if newAdmission.Replay.authority == fragmentAdmission.Replay.authority {
		t.Fatal("same ID reuse retained expired generation authority")
	}
	newAdmission.pin.release()
	if _, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, fragment, fragmentAdmission.Replay,
	); err != nil {
		t.Fatalf("old permit crossed same-ID generation with lane error: %v", err)
	}
	if _, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, conflictingFinal, finalAdmission.Replay,
	); err != nil {
		t.Fatalf("stale final permit caused a lane error: %v", err)
	}
}

func TestOutboundReplayPermitCannotCrossCancellationGenerationOrTerminal(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	operationID := testOperationID(102)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	authority := observeSenderOperation(t, table, request)
	fragment := mustFragmentMessage(t, operationID, 1)
	admission, _ := table.AdmitOutbound(DirectionSenderToReceiver, fragment, authority)
	if err := table.CancelGeneration(authority.Generation()); err != nil {
		t.Fatal(err)
	}
	if disposition, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, fragment, admission.Replay,
	); err != nil || disposition.Disposition != OperationDrop {
		t.Fatalf("cancelled replay=%v error=%v", disposition, err)
	}
	admission.pin.release()

	secondID := testOperationID(103)
	secondRequest := mustMessage(t, MessageRequestBlocks, &secondID, map[uint64]any{0: uint64(2)})
	secondAuthority := observeSenderOperation(t, table, secondRequest)
	secondFragment := mustFragmentMessage(t, secondID, 2)
	secondAdmission, _ := table.AdmitOutbound(DirectionSenderToReceiver, secondFragment, secondAuthority)
	if admission.Replay.authority == secondAdmission.Replay.authority {
		t.Fatal("distinct operations shared replay authority")
	}
	secondAdmission.pin.release()
	if _, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, secondFragment, admission.Replay,
	); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("cross-generation replay error=%v", err)
	}
	terminal := mustMessage(t, MessageSessionTerminal, nil, map[uint64]any{0: uint64(1)})
	_, _ = table.Observe(DirectionSenderToReceiver, terminal)
	if disposition, err := table.AcceptOutboundReplay(
		DirectionSenderToReceiver, secondFragment, OutboundReplayPermit{},
	); err != nil || disposition.Disposition != OperationDrop {
		t.Fatalf("terminal replay=%v error=%v", disposition, err)
	}
}

func TestReceiverRequestReplayRequiresExactGenerationPermit(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(104)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	admission, err := table.AdmitOutbound(DirectionReceiverToSender, request, OutboundOperationPermit{})
	if err != nil || admission.Disposition != OperationDeliver || admission.Replay.IsZero() {
		t.Fatalf("request admission=%+v error=%v", admission, err)
	}
	replayed, err := table.AcceptOutboundReplay(
		DirectionReceiverToSender,
		request,
		admission.Replay,
	)
	if err != nil || replayed.Disposition != OperationDeliver ||
		!replayed.Generation.Same(admission.Generation) {
		t.Fatalf("exact request replay=%+v error=%v", replayed, err)
	}
	replayed.pin.release()
	conflicting := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.AcceptOutboundReplay(
		DirectionReceiverToSender,
		conflicting,
		admission.Replay,
	); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("conflicting request replay error=%v", err)
	}
	admission.pin.release()
}

func TestPeerAnswerReplayDoesNotAdvanceMultiplicityTwice(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(105)
	request := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
	authority := observeSenderOperation(t, table, request)
	answer := mustMessage(t, MessagePeerAnswer, &operationID, map[uint64]any{0: uint64(2)})
	admission, err := table.AdmitOutbound(DirectionSenderToReceiver, answer, authority)
	if err != nil || admission.Replay.IsZero() {
		t.Fatalf("answer admission=%+v error=%v", admission, err)
	}
	for replay := range 2 {
		replayed, err := table.AcceptOutboundReplay(
			DirectionSenderToReceiver, answer, admission.Replay,
		)
		if err != nil || replayed.Disposition != OperationDeliver {
			t.Fatalf("answer replay %d=%v error=%v", replay, replayed.Disposition, err)
		}
		replayed.pin.release()
	}
	admission.pin.release()
	if disposition, err := table.Observe(
		DirectionSenderToReceiver,
		answer,
	); err != nil || disposition != OperationDrop {
		t.Fatalf("exact duplicate answer was not idempotent: disposition=%d err=%v", disposition, err)
	}
}

func observeSenderOperation(
	t *testing.T,
	table *OperationTable,
	request Message,
) OutboundOperationPermit {
	t.Helper()
	admission, err := table.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil || admission.Disposition != OperationDeliver || admission.Outbound.IsZero() {
		t.Fatalf("inbound operation admission=%+v error=%v", admission, err)
	}
	return admission.Outbound
}
