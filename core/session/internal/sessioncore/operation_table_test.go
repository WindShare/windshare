package sessioncore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestOperationTableLifecycleContinuationAndInFlightPinning(t *testing.T) {
	now := time.Unix(50_000, 0)
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2},
		func() time.Time { return now },
		continuationReplayClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	id := id16[protocolsession.OperationID](10)
	offer := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})

	// Receiver outbound admission + pin prevents premature tombstone expiry.
	var violated protocolsession.AuthenticatedOperationViolation
	op, maxCont, hasLimit, release, err := table.AdmitOutboundRequest(offer, func(v protocolsession.AuthenticatedOperationViolation) {
		violated = v
	})
	if err != nil || !op.IsActive() || !op.IsCurrent() || !hasLimit || maxCont != 4 {
		t.Fatalf("admit outbound: op=%v active=%v limit=%d/%v err=%v", op, op.IsActive(), maxCont, hasLimit, err)
	}
	if kind, ok := op.RequestKind(); !ok || kind != protocolsession.MessagePeerOffer {
		t.Fatalf("request kind=%d ok=%v", kind, ok)
	}
	if limit, ok := table.MaximumContinuations(op.Ref()); !ok || limit != 4 {
		t.Fatalf("maximum continuations=%d ok=%v", limit, ok)
	}

	// Duplicate PeerAnswer is dropped; conflicting PeerAnswer notifies violation handler.
	answer1 := operationMessageForTest(t, protocolsession.MessagePeerAnswer, id, []byte{0xa1, 0x00, 0x01})
	adm, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, answer1)
	if err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("first answer=%+v err=%v", adm, err)
	}
	adm, err = table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, answer1)
	if err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("duplicate answer=%+v err=%v", adm, err)
	}
	answer2 := operationMessageForTest(t, protocolsession.MessagePeerAnswer, id, []byte{0xa1, 0x00, 0x02})
	if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, answer2); !errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("conflicting answer err=%v", err)
	}
	if !violated.Valid() || violated.Code() != protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer {
		t.Fatalf("violation notification=%+v", violated)
	}

	// Cancel transitions active op to tombstone; repeated cancel is idempotent no-op.
	transitioned, err := table.CancelOperation(op.Ref())
	if err != nil || !transitioned || op.IsActive() || !op.IsCurrent() {
		t.Fatalf("first cancel: transitioned=%v active=%v current=%v err=%v", transitioned, op.IsActive(), op.IsCurrent(), err)
	}
	transitioned, err = table.CancelOperation(op.Ref())
	if err != nil || transitioned {
		t.Fatalf("second cancel: transitioned=%v err=%v", transitioned, err)
	}

	// While inFlight pin is held, advancing clock past tombstone lifetime MUST NOT evict.
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Second)
	if table.TombstoneCount() != 1 {
		t.Fatalf("pinned tombstone evicted early: count=%d", table.TombstoneCount())
	}
	// Releasing with refresh=true grants a full lifetime from release time.
	release(true)
	if table.TombstoneCount() != 1 {
		t.Fatalf("refreshed tombstone missing immediately after release")
	}
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	if table.TombstoneCount() != 0 {
		t.Fatalf("expired unpinned tombstone retained: count=%d", table.TombstoneCount())
	}
}

func TestOperationTablePreemptiveCancelAndContinuationRollback(t *testing.T) {
	now := time.Unix(60_000, 0)
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTracked: 4},
		func() time.Time { return now },
		continuationReplayClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	id := id16[protocolsession.OperationID](11)

	// Cancel arrives before the request (preemptive cancel), followed by late candidate and late offer.
	cancelMsg := operationMessageForTest(t, protocolsession.MessageCancel, id, []byte{0xa0})
	adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, cancelMsg)
	if err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("preemptive cancel=%+v err=%v", adm, err)
	}
	cand := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xf6})
	adm, err = table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, cand)
	if err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("unbound late candidate=%+v err=%v", adm, err)
	}
	offer := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})
	adm, err = table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer)
	if err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("matching late offer=%+v err=%v", adm, err)
	}

	// Outbound continuation undo rolls back unsent candidate so an exact retry succeeds.
	id2 := id16[protocolsession.OperationID](12)
	offer2 := operationMessageForTest(t, protocolsession.MessagePeerOffer, id2, []byte{0xf6})
	op2, _, _, releaseOffer, err := table.AdmitOutboundRequest(offer2, nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseOffer(false)
	cand2 := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id2, []byte{0xf6})
	disp, undo, relCont, err := table.AdmitOutboundContinuation(op2.Ref(), cand2)
	if err != nil || disp != protocolsession.OperationDeliver || undo == nil {
		t.Fatalf("first outbound continuation: disp=%d err=%v", disp, err)
	}
	undo()
	relCont(false)
	disp, _, relCont2, err := table.AdmitOutboundContinuation(op2.Ref(), cand2)
	if err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("retry after undo: disp=%d err=%v", disp, err)
	}
	relCont2(true)
	// Exact duplicate after commit is dropped idempotently.
	disp, _, _, err = table.AdmitOutboundContinuation(op2.Ref(), cand2)
	if err != nil || disp != protocolsession.OperationDrop {
		t.Fatalf("duplicate after commit: disp=%d err=%v", disp, err)
	}
}

func TestInboundQueueOrderingDataOverflowAndDeferredPromotion(t *testing.T) {
	now := time.Unix(70_000, 0)
	ops, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 1, MaxTracked: 2},
		func() time.Time { return now },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Receiver side: final must follow queued data for the same operation.
	rxQueue, err := newInboundQueue(nil, protocolsession.RoleReceiver, ops, protocolsession.RouterLimits{
		ControlFrames: 4, DataFrames: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rxQueue.Close()
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	id := id16[protocolsession.OperationID](21)
	req := operationMessageForTest(t, protocolsession.MessageRequestBlocks, id, []byte{0xa0})
	_, _, _, relReq, err := ops.AdmitOutboundRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	relReq(false)

	frags, err := contentflow.FragmentRecord(id, []byte{0x01, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	frag := frags[0]
	final := operationMessageForTest(t, protocolsession.MessageOperationComplete, id, []byte{0xa0})
	if disp, err := rxQueue.RouteInbound(lane, frag); err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("route frag: disp=%d err=%v", disp, err)
	}
	if disp, err := rxQueue.RouteInbound(lane, final); err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("route final: disp=%d err=%v", disp, err)
	}
	first, err := rxQueue.Next(context.Background())
	if err != nil || first.message.Kind() != protocolsession.MessageBlockFragment {
		t.Fatalf("first event=%+v err=%v, want fragment before final", first.message.Kind(), err)
	}
	second, err := rxQueue.Next(context.Background())
	if err != nil || second.message.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("second event=%+v err=%v, want final after fragment", second.message.Kind(), err)
	}

	// Data overflow cancels operation and emits ErrRouterDataFull event on control channel.
	ops2, _ := newOperationTable(protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 4}, func() time.Time { return now }, nil)
	rxOverflow, _ := newInboundQueue(nil, protocolsession.RoleReceiver, ops2, protocolsession.RouterLimits{
		ControlFrames: 4, DataFrames: 1,
	})
	defer rxOverflow.Close()
	idOverflow := id16[protocolsession.OperationID](22)
	reqOv := operationMessageForTest(t, protocolsession.MessageRequestBlocks, idOverflow, []byte{0xa0})
	opOv, _, _, relOv, _ := ops2.AdmitOutboundRequest(reqOv, nil)
	relOv(false)
	ovFrags, err := contentflow.FragmentRecord(idOverflow, []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	fragOv := ovFrags[0]
	if _, err := rxOverflow.RouteInbound(lane, fragOv); err != nil {
		t.Fatal(err)
	}
	if disp, err := rxOverflow.RouteInbound(lane, fragOv); err != nil || disp != protocolsession.OperationDrop {
		t.Fatalf("overflow frag: disp=%d err=%v", disp, err)
	}
	if opOv.IsActive() {
		t.Fatal("overflow did not cancel operation")
	}
	ovEvent, err := rxOverflow.Next(context.Background())
	if err != nil || !errors.Is(ovEvent.err, protocolsession.ErrRouterDataFull) {
		t.Fatalf("overflow event err=%v want ErrRouterDataFull", ovEvent.err)
	}

	// Sender side: deferred request promotes in FIFO order once active slot frees.
	txOps, _ := newOperationTable(protocolsession.OperationLimits{MaxActive: 1, MaxTracked: 2}, func() time.Time { return now }, nil)
	txQueue, _ := newInboundQueue(nil, protocolsession.RoleSender, txOps, protocolsession.RouterLimits{
		ControlFrames: 4, DataFrames: 4,
	})
	defer txQueue.Close()
	req1ID := id16[protocolsession.OperationID](31)
	req2ID := id16[protocolsession.OperationID](32)
	req1 := operationMessageForTest(t, protocolsession.MessageListChildren, req1ID, []byte{0xa0})
	req2 := operationMessageForTest(t, protocolsession.MessageListChildren, req2ID, []byte{0xa0})
	if _, err := txQueue.RouteInbound(lane, req1); err != nil {
		t.Fatal(err)
	}
	if disp, err := txQueue.RouteInbound(lane, req2); err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("deferred req2: disp=%d err=%v", disp, err)
	}
	ev1, err := txQueue.Next(context.Background())
	if err != nil || ev1.op.ID() != req1ID {
		t.Fatalf("first promoted op=%v err=%v", ev1.op.ID(), err)
	}
	if _, err := txOps.CancelOperation(ev1.op.Ref()); err != nil {
		t.Fatal(err)
	}
	ev2, err := txQueue.Next(context.Background())
	if err != nil || ev2.op.ID() != req2ID {
		t.Fatalf("deferred promoted op=%v err=%v", ev2.op.ID(), err)
	}
}

type peerSlotTestClassifier struct{}

type peerSlotTestContract struct {
	binding protocolsession.PeerAttemptBinding
}

func (c peerSlotTestContract) PeerAttemptBinding() protocolsession.PeerAttemptBinding {
	return c.binding
}

func (c peerSlotTestContract) OperationContinuationScope() protocolsession.OperationContinuationScope {
	var scope protocolsession.OperationContinuationScope
	copy(scope[:16], c.binding.PeerPathID[:])
	copy(scope[16:], c.binding.AttemptID[:])
	return scope
}

func (c peerSlotTestContract) MaximumContinuations() int { return 8 }

func (c peerSlotTestContract) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) ([32]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [32]byte{}, false, nil
	}
	binding, ok, err := (peerSlotTestClassifier{}).ClassifyPeerAttemptContinuation(kind, body)
	if err != nil || !ok {
		return [32]byte{}, ok, err
	}
	if binding != c.binding {
		return [32]byte{}, true, protocolsession.ErrConflictingContinuation
	}
	var key [32]byte
	copy(key[:], body)
	return key, true, nil
}

func encodeTestPeerBindingBody(t *testing.T, binding protocolsession.PeerAttemptBinding) []byte {
	t.Helper()
	body, err := protocolsession.EncodeBody([]any{
		binding.PeerPathID[:], binding.AttemptID[:], binding.AttemptSequence, uint64(binding.PeerSlot),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (peerSlotTestClassifier) decode(body []byte) (protocolsession.PeerAttemptBinding, error) {
	var raw []any
	if err := cbor.Unmarshal(body, &raw); err != nil || len(raw) != 4 {
		return protocolsession.PeerAttemptBinding{}, protocolsession.ErrInvalidMessage
	}
	path, ok1 := raw[0].([]byte)
	attempt, ok2 := raw[1].([]byte)
	seq, ok3 := raw[2].(uint64)
	slot, ok4 := raw[3].(uint64)
	if !ok1 || !ok2 || !ok3 || !ok4 || len(path) != 16 || len(attempt) != 16 {
		return protocolsession.PeerAttemptBinding{}, protocolsession.ErrInvalidMessage
	}
	return protocolsession.PeerAttemptBinding{
		PeerSlot: uint8(slot), PeerPathID: [16]byte(path), AttemptID: [16]byte(attempt), AttemptSequence: seq,
	}, nil
}

func (c peerSlotTestClassifier) BeginOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationContract, bool, error) {
	if kind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	binding, err := c.decode(body)
	if err != nil {
		return nil, true, err
	}
	return peerSlotTestContract{binding: binding}, true, nil
}

func (c peerSlotTestClassifier) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return protocolsession.OperationContinuationScope{}, false, nil
	}
	binding, err := c.decode(body)
	if err != nil {
		return protocolsession.OperationContinuationScope{}, true, err
	}
	return peerSlotTestContract{binding: binding}.OperationContinuationScope(), true, nil
}

func (c peerSlotTestClassifier) ClassifyPeerAttemptContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.PeerAttemptBinding, bool, error) {
	switch kind {
	case protocolsession.MessagePeerOffer, protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate:
		binding, err := c.decode(body)
		return binding, true, err
	case protocolsession.MessageOperationError:
		failure, err := protocolsession.DecodeOperationFailure(body)
		if err != nil || failure.PeerAttempt == nil {
			return protocolsession.PeerAttemptBinding{}, false, err
		}
		return *failure.PeerAttempt, true, nil
	default:
		return protocolsession.PeerAttemptBinding{}, false, nil
	}
}

func TestRetiredPeerContinuationSurvivesOperationGC(t *testing.T) {
	now := time.Unix(1, 0)
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTracked: 4},
		func() time.Time { return now },
		peerSlotTestClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	binding := protocolsession.PeerAttemptBinding{
		PeerSlot: 1, PeerPathID: [16]byte{1}, AttemptID: [16]byte{2}, AttemptSequence: 1,
	}
	offer := func(id protocolsession.OperationID, b protocolsession.PeerAttemptBinding) protocolsession.Message {
		return operationMessageForTest(t, protocolsession.MessagePeerOffer, id, encodeTestPeerBindingBody(t, b))
	}
	cont := func(id protocolsession.OperationID, b protocolsession.PeerAttemptBinding, kind protocolsession.MessageKind) protocolsession.Message {
		if kind == protocolsession.MessageOperationError {
			pb := b
			body, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
				Scope: protocolsession.OperationScopePeer, Code: protocolsession.PeerOperationCodeAuthentication,
				Message: "late failure", PeerAttempt: &pb,
			})
			if err != nil {
				t.Fatal(err)
			}
			return operationMessageForTest(t, kind, id, body)
		}
		return operationMessageForTest(t, kind, id, encodeTestPeerBindingBody(t, b))
	}

	oldID := protocolsession.OperationID{1}
	admitted, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(oldID, binding))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.CancelOperation(admitted.op.Ref()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Millisecond)

	current := binding
	current.AttemptSequence++
	current.AttemptID[0]++
	currentID := protocolsession.OperationID{2}
	currentAdm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(currentID, current))
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate, protocolsession.MessageOperationError,
	} {
		if adm, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(oldID, binding, kind)); err != nil || adm.disposition != protocolsession.OperationDrop {
			t.Fatalf("late %d: disp=%d err=%v", kind, adm.disposition, err)
		}
		unknown := protocolsession.OperationID{3}
		future := current
		future.AttemptSequence++
		if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(unknown, future, kind)); !errors.Is(err, protocolsession.ErrUnknownOperation) {
			t.Fatalf("unknown future %d: %v", kind, err)
		}
		if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(currentID, binding, kind)); !errors.Is(err, protocolsession.ErrConflictingContinuation) {
			t.Fatalf("conflicting current %d: %v", kind, err)
		}
	}
	// Superseded offer drops even after tombstone eviction.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(protocolsession.OperationID{4}, binding)); err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("superseded offer: disp=%d err=%v", adm.disposition, err)
	}
	// Advancing 128 sequences must preserve protection for older active currentAdm.
	for seq := uint64(3); seq < 131; seq++ {
		next := binding
		next.AttemptSequence = seq
		next.AttemptID[0] = byte(seq)
		adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(protocolsession.OperationID{byte(seq), 1}, next))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := table.CancelOperation(adm.op.Ref()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(protocolsession.OperationTombstoneLifetime + time.Millisecond)
	}
	wrongID := protocolsession.OperationID{250}
	if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(wrongID, current, protocolsession.MessageOperationError)); !errors.Is(err, protocolsession.ErrUnknownOperation) {
		t.Fatalf("active older sequence lost protection: %v", err)
	}
	if _, err := table.CancelOperation(currentAdm.op.Ref()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Millisecond)
	if adm, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(wrongID, current, protocolsession.MessageOperationError)); err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("retired older sequence not collected: disp=%d err=%v", adm.disposition, err)
	}
}
