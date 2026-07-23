package protocolsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

func TestMessageCanonicalRoundTripAndOwnership(t *testing.T) {
	body, err := EncodeBody(map[uint64]any{0: uint64(1), 1: "ready", 2: []any{uint64(2), []byte{3}}})
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(1)
	for kind := MessageListChildren; kind <= MessagePeerCandidate; kind++ {
		if kind == MessageBlockFragment {
			continue
		}
		var id *OperationID
		if kind != MessageSessionTerminal {
			id = &operationID
		}
		message, err := NewMessage(kind, id, body)
		if err != nil {
			t.Fatalf("new kind %d: %v", kind, err)
		}
		plaintext, err := EncodeMessage(message)
		if err != nil {
			t.Fatalf("encode kind %d: %v", kind, err)
		}
		decoded, err := DecodeMessage(plaintext)
		if err != nil {
			t.Fatalf("decode kind %d: %v", kind, err)
		}
		if decoded.Kind() != kind || decoded.IsData() || !bytes.Equal(decoded.Body(), body) {
			t.Fatalf("kind %d round trip changed message", kind)
		}
		gotID, hasID := decoded.OperationID()
		if (kind != MessageSessionTerminal) != hasID || (hasID && gotID != operationID) {
			t.Fatalf("kind %d operation identity = %x, %v", kind, gotID, hasID)
		}
		plaintext[0] ^= 1
		copyBody := decoded.Body()
		copyBody[0] ^= 1
		stable, _ := EncodeMessage(decoded)
		if bytes.Equal(stable, plaintext) || bytes.Equal(decoded.Body(), copyBody) {
			t.Fatalf("kind %d exposed mutable storage", kind)
		}
	}
}

func TestPeerSignalingMessageKindsMatchSharedVector(t *testing.T) {
	var vector struct {
		MessageKinds struct {
			Offer     uint8 `json:"offer"`
			Answer    uint8 `json:"answer"`
			Candidate uint8 `json:"candidate"`
		} `json:"messageKinds"`
	}
	encoded, err := os.ReadFile(filepath.Join("..", "..", "testvectors", "v2-peer-signaling.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &vector); err != nil {
		t.Fatal(err)
	}
	if vector.MessageKinds.Offer != uint8(MessagePeerOffer) ||
		vector.MessageKinds.Answer != uint8(MessagePeerAnswer) ||
		vector.MessageKinds.Candidate != uint8(MessagePeerCandidate) {
		t.Fatalf("shared peer signaling registry = %+v", vector.MessageKinds)
	}
}

func TestMessageRejectsHostileBodiesAndOuterEncodings(t *testing.T) {
	operationID := testOperationID(2)
	canonical := mustBody(t, map[uint64]any{0: uint64(1)})
	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{"unknown kind", func() error { _, err := NewMessage(99, &operationID, canonical); return err }, ErrUnknownMessageKind},
		{"fragment through cbor", func() error { _, err := NewMessage(MessageBlockFragment, &operationID, canonical); return err }, ErrInvalidMessage},
		{"terminal with operation", func() error { _, err := NewMessage(MessageSessionTerminal, &operationID, canonical); return err }, ErrInvalidOperationID},
		{"operation without identity", func() error { _, err := NewMessage(MessageListChildren, nil, canonical); return err }, ErrInvalidOperationID},
		{"zero operation", func() error {
			zero := OperationID{}
			_, err := NewMessage(MessageListChildren, &zero, canonical)
			return err
		}, ErrInvalidOperationID},
		{"empty body", func() error { _, err := NewMessage(MessageListChildren, &operationID, nil); return err }, ErrInvalidMessage},
		{"noncanonical integer", func() error { _, err := NewMessage(MessageListChildren, &operationID, []byte{0x18, 0x01}); return err }, ErrNonCanonicalMessage},
		{"negative body", func() error { _, err := EncodeBody(int64(-1)); return err }, ErrInvalidMessage},
		{"float body", func() error { _, err := EncodeBody(1.5); return err }, ErrInvalidMessage},
		{"non nfc text", func() error { _, err := EncodeBody("e\u0301"); return err }, ErrInvalidMessage},
		{"string map key", func() error { _, err := EncodeBody(map[string]any{"x": uint64(1)}); return err }, ErrInvalidMessage},
		{"empty plaintext", func() error { _, err := DecodeMessage(nil); return err }, ErrInvalidMessage},
		{"oversize plaintext", func() error { _, err := DecodeMessage(make([]byte, MaxEnvelopePlaintextBytes+1)); return err }, ErrMessageTooLarge},
		{"malformed plaintext", func() error { _, err := DecodeMessage([]byte{0xff}); return err }, ErrInvalidMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}

	outer, err := messageEncMode.Marshal(map[uint64]any{0: uint64(MessageListChildren), 1: nil, 2: map[uint64]any{0: uint64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeMessage(outer); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("nil operation identity: %v", err)
	}
	shortID, _ := messageEncMode.Marshal(map[uint64]any{0: uint64(MessageListChildren), 1: []byte{1}, 2: map[uint64]any{0: uint64(1)}})
	if _, err := DecodeMessage(shortID); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("short operation identity: %v", err)
	}
	extra, _ := messageEncMode.Marshal(map[uint64]any{0: uint64(MessageListChildren), 1: operationID.Bytes(), 2: map[uint64]any{0: uint64(1)}, 3: uint64(0)})
	if _, err := DecodeMessage(extra); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("extra outer field: %v", err)
	}
	if _, err := EncodeMessage(Message{}); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("zero message encode: %v", err)
	}
}

func TestFragmentMessageRoutingHeaderIsBoundedAndOwned(t *testing.T) {
	operationID := testOperationID(3)
	fragment := make([]byte, fragmentRoutingHeaderBytes+3)
	fragment[0], fragment[1], fragment[2] = 1, byte(MessageBlockFragment), 1
	copy(fragment[4:fragmentRoutingHeaderBytes], operationID[:])
	copy(fragment[fragmentRoutingHeaderBytes:], []byte{7, 8, 9})
	message, err := DecodeMessage(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if !message.IsData() || message.Kind() != MessageBlockFragment {
		t.Fatal("fragment was not classified as data")
	}
	fragment[4] ^= 1
	encoded, err := EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if encoded[4] != operationID[0] {
		t.Fatal("fragment retained caller-owned bytes")
	}
	for name, candidate := range map[string][]byte{
		"short":         encoded[:fragmentRoutingHeaderBytes-1],
		"flags":         mutateBytes(encoded, func(value []byte) { value[2] = 2 }),
		"reserved":      mutateBytes(encoded, func(value []byte) { value[3] = 1 }),
		"zero identity": mutateBytes(encoded, func(value []byte) { clear(value[4:fragmentRoutingHeaderBytes]) }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMessage(candidate); err == nil {
				t.Fatal("hostile fragment was accepted")
			}
		})
	}
}

func TestOperationTableEnforcesMultiplicityCancellationAndTerminal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	table, err := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(10)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	progress := mustMessage(t, MessageScanProgress, &operationID, map[uint64]any{0: uint64(1)})
	final := mustMessage(t, MessageCatalogResult, &operationID, map[uint64]any{0: uint64(1)})
	requestAdmission, err := table.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil || requestAdmission.Disposition != OperationDeliver {
		t.Fatalf("begin = %v, %v", requestAdmission.Disposition, err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, progress); err != nil || disposition != OperationDeliver {
		t.Fatalf("progress = %v, %v", disposition, err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDeliver {
		t.Fatalf("final = %v, %v", disposition, err)
	}
	if table.ActiveCount() != 0 || table.TombstoneCount() != 1 {
		t.Fatalf("counts = active %d tombstones %d", table.ActiveCount(), table.TombstoneCount())
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDrop {
		t.Fatalf("duplicate final = %v, %v", disposition, err)
	}
	conflict := mustMessage(t, MessageCatalogResult, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.Observe(DirectionSenderToReceiver, conflict); !errors.Is(err, ErrConflictingFinal) {
		t.Fatalf("conflicting final: %v", err)
	}
	if err := table.CancelGeneration(requestAdmission.Generation); err != nil {
		t.Fatalf("cancel after final: %v", err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDrop {
		t.Fatalf("final after local cancellation race = %v, %v", disposition, err)
	}
	if _, err := table.Observe(DirectionSenderToReceiver, conflict); !errors.Is(err, ErrConflictingFinal) {
		t.Fatalf("local cancellation overwrote final authority: %v", err)
	}
	if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDrop {
		t.Fatalf("exact request replay = %v, %v", disposition, err)
	}
	conflictingRequest := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.Observe(DirectionReceiverToSender, conflictingRequest); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("conflicting request reuse: %v", err)
	}

	cancelID := testOperationID(11)
	cancel := mustMessage(t, MessageCancel, &cancelID, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionReceiverToSender, cancel); err != nil || disposition != OperationDeliver {
		t.Fatalf("cancel-before-request = %v, %v", disposition, err)
	}
	lateRequest := mustMessage(t, MessageRequestBlocks, &cancelID, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionReceiverToSender, lateRequest); err != nil || disposition != OperationDrop {
		t.Fatalf("late request = %v, %v", disposition, err)
	}

	localID := testOperationID(12)
	localRequest := mustMessage(t, MessageRequestBlocks, &localID, map[uint64]any{0: uint64(1)})
	localAdmission, _ := table.ObserveInbound(DirectionReceiverToSender, localRequest)
	if err := table.CancelGeneration(localAdmission.Generation); err != nil {
		t.Fatal(err)
	}
	fragment := mustFragmentMessage(t, localID, 1)
	if disposition, err := table.Observe(DirectionSenderToReceiver, fragment); err != nil || disposition != OperationDrop {
		t.Fatalf("late fragment = %v, %v", disposition, err)
	}

	terminal := mustMessage(t, MessageSessionTerminal, nil, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionSenderToReceiver, terminal); err != nil || disposition != OperationSessionTerminal {
		t.Fatalf("terminal = %v, %v", disposition, err)
	}
	if !table.Terminated() || table.ActiveCount() != 0 || table.TombstoneCount() != 0 {
		t.Fatal("terminal did not clear operation state")
	}
	if disposition, err := table.Observe(DirectionReceiverToSender, request); err != nil || disposition != OperationDrop {
		t.Fatalf("post-terminal traffic = %v, %v", disposition, err)
	}
	if err := table.TerminateLocal(); !errors.Is(err, ErrSessionTerminated) {
		t.Fatalf("duplicate local terminal: %v", err)
	}
}

func TestOperationTableRejectsWrongKindsAndBudgets(t *testing.T) {
	if _, err := NewOperationTable(OperationLimits{}, nil); err == nil {
		t.Fatal("invalid limits were accepted")
	}
	if _, err := (Role(99)).InboundDirection(); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid inbound role: %v", err)
	}
	if _, err := (Role(99)).OutboundDirection(); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid outbound role: %v", err)
	}
	if direction, _ := RoleReceiver.OutboundDirection(); direction != DirectionReceiverToSender {
		t.Fatalf("receiver outbound direction = %d", direction)
	}

	now := time.Unix(1_700_000_000, 0)
	table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTombstones: 1}, func() time.Time { return now })
	firstID, secondID := testOperationID(20), testOperationID(21)
	first := mustMessage(t, MessageOpenRevisions, &firstID, map[uint64]any{0: uint64(1)})
	second := mustMessage(t, MessageRenewLease, &secondID, map[uint64]any{0: uint64(1)})
	_, _ = table.Observe(DirectionReceiverToSender, first)
	if _, err := table.Observe(DirectionReceiverToSender, second); !errors.Is(err, ErrActiveOperationBudget) {
		t.Fatalf("active budget: %v", err)
	}
	wrongResponse := mustMessage(t, MessageLeaseResult, &firstID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionSenderToReceiver, wrongResponse); !errors.Is(err, ErrUnexpectedOperation) {
		t.Fatalf("wrong response: %v", err)
	}
	openFinal := mustMessage(t, MessageOpenResults, &firstID, map[uint64]any{0: uint64(1)})
	_, _ = table.Observe(DirectionSenderToReceiver, openFinal)
	_, _ = table.Observe(DirectionReceiverToSender, second)
	leaseFinal := mustMessage(t, MessageLeaseResult, &secondID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionSenderToReceiver, leaseFinal); !errors.Is(err, ErrTombstoneBudget) {
		t.Fatalf("tombstone budget: %v", err)
	}
	if err := table.CancelGeneration(OperationGeneration{}); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("zero local cancel: %v", err)
	}
	unknown := mustMessage(t, MessageOperationComplete, &testIDHolder, map[uint64]any{0: uint64(0)})
	if _, err := table.Observe(DirectionSenderToReceiver, unknown); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("unknown response: %v", err)
	}
	if _, err := table.Observe(DirectionSenderToReceiver, first); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("wrong request direction: %v", err)
	}

	now = now.Add(OperationTombstoneLifetime)
	if table.TombstoneCount() != 0 {
		t.Fatal("expired tombstone retained budget")
	}
	if err := (*OperationTable)(nil).TerminateLocal(); err == nil {
		t.Fatal("nil operation table terminated")
	}
}

func TestOperationTableAcceptsEveryFrozenRequestResponseShape(t *testing.T) {
	tests := []struct {
		name         string
		request      MessageKind
		intermediate MessageKind
		final        MessageKind
	}{
		{"catalog", MessageListChildren, MessageScanProgress, MessageCatalogResult},
		{"open", MessageOpenRevisions, 0, MessageOpenResults},
		{"renew", MessageRenewLease, 0, MessageLeaseResult},
		{"release", MessageReleaseLease, 0, MessageOperationComplete},
		{"blocks", MessageRequestBlocks, MessageBlockFragment, MessageOperationComplete},
		{"lane attach", MessageLaneAttach, 0, MessageLaneAttach},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
			operationID := testOperationID(byte(100 + index))
			request := mustMessage(t, test.request, &operationID, map[uint64]any{0: uint64(1)})
			if _, err := table.Observe(DirectionReceiverToSender, request); err != nil {
				t.Fatal(err)
			}
			if test.intermediate != 0 {
				var message Message
				if test.intermediate == MessageBlockFragment {
					message = mustFragmentMessage(t, operationID, 1)
				} else {
					message = mustMessage(t, test.intermediate, &operationID, map[uint64]any{0: uint64(1)})
				}
				if disposition, err := table.Observe(DirectionSenderToReceiver, message); err != nil || disposition != OperationDeliver {
					t.Fatalf("intermediate = %v, %v", disposition, err)
				}
			}
			final := mustMessage(t, test.final, &operationID, map[uint64]any{0: uint64(1)})
			if disposition, err := table.Observe(DirectionSenderToReceiver, final); err != nil || disposition != OperationDeliver {
				t.Fatalf("final = %v, %v", disposition, err)
			}
		})
	}

	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(120)
	request := mustMessage(t, MessageOpenRevisions, &operationID, map[uint64]any{0: uint64(1)})
	_, _ = table.Observe(DirectionReceiverToSender, request)
	operationError := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
	if disposition, err := table.Observe(DirectionSenderToReceiver, operationError); err != nil || disposition != OperationDeliver {
		t.Fatalf("operation error = %v, %v", disposition, err)
	}
	if _, err := (*OperationTable)(nil).Observe(DirectionReceiverToSender, request); err == nil {
		t.Fatal("nil table accepted a message")
	}
}

func TestPeerSignalingOperationBindsOneOfferAndBidirectionalCandidates(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(126)
	offer := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
	answer := mustMessage(t, MessagePeerAnswer, &operationID, map[uint64]any{0: uint64(1)})
	candidate := mustMessage(t, MessagePeerCandidate, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionReceiverToSender, offer); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Observe(DirectionReceiverToSender, candidate); err != nil {
		t.Fatalf("offerer candidate: %v", err)
	}
	if _, err := table.Observe(DirectionSenderToReceiver, answer); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if _, err := table.Observe(DirectionSenderToReceiver, candidate); err != nil {
		t.Fatalf("answerer candidate: %v", err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, answer); err != nil || disposition != OperationDrop {
		t.Fatalf("exact answer replay = %v, %v", disposition, err)
	}
	conflictingAnswer := mustMessage(t, MessagePeerAnswer, &operationID, map[uint64]any{0: uint64(2)})
	if _, err := table.Observe(DirectionSenderToReceiver, conflictingAnswer); !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("conflicting answer reuse: %v", err)
	}
	cancel := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionReceiverToSender, cancel); err != nil {
		t.Fatalf("signaling completion cancel: %v", err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, candidate); err != nil || disposition != OperationDrop {
		t.Fatalf("late candidate = %v, %v", disposition, err)
	}

	wrongID := testOperationID(125)
	wrongOffer := mustMessage(t, MessagePeerOffer, &wrongID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionSenderToReceiver, wrongOffer); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("sender-originated offer error = %v", err)
	}
	wrongAnswer := mustMessage(t, MessagePeerAnswer, &wrongID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionReceiverToSender, wrongAnswer); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("receiver-originated answer error = %v", err)
	}
}

func TestOperationTableTreatsResignedSemanticFinalAsIdempotent(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(127)
	request := mustMessage(t, MessageOpenRevisions, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := table.Observe(DirectionReceiverToSender, request); err != nil {
		t.Fatal(err)
	}
	prepared := mustPreparedControl(t, MessageOpenResults, &operationID)
	first, err := prepared.build(11)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepared.build(12)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Body(), second.Body()) {
		t.Fatal("different delivery sequences unexpectedly produced the same signature")
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, first); err != nil || disposition != OperationDeliver {
		t.Fatalf("first final = %d, %v", disposition, err)
	}
	if disposition, err := table.Observe(DirectionSenderToReceiver, second); err != nil || disposition != OperationDrop {
		t.Fatalf("resigned semantic duplicate = %d, %v", disposition, err)
	}
}

var testIDHolder = testOperationID(99)

func TestRoleRouterBoundsDispatchAndTerminal(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 8, MaxTombstones: 8}, nil)
	if _, err := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{}); err == nil {
		t.Fatal("invalid router limits were accepted")
	}
	if _, err := NewRoleRouter(Role(99), table); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid router role: %v", err)
	}
	router, err := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{ControlFrames: 1, DataFrames: 1})
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(30)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	if disposition, err := router.RouteInbound(context.Background(), request); err != nil || disposition != OperationDeliver {
		t.Fatalf("route request = %v, %v", disposition, err)
	}
	event := <-router.control
	var handled atomic.Int32
	handler := MessageHandlerFunc(func(_ context.Context, message Message) error {
		handled.Add(1)
		if message.Kind() != MessageListChildren {
			return errors.New("wrong kind")
		}
		return nil
	})
	if err := router.RegisterHandler(MessageListChildren, handler); err != nil {
		t.Fatal(err)
	}
	if err := router.RegisterHandler(MessageListChildren, handler); !errors.Is(err, ErrHandlerRegistered) {
		t.Fatalf("duplicate handler: %v", err)
	}
	if err := router.RegisterHandler(MessageCatalogResult, handler); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("wrong-direction handler: %v", err)
	}
	if err := router.Dispatch(context.Background(), event); err != nil || handled.Load() != 1 {
		t.Fatalf("dispatch = calls %d err %v", handled.Load(), err)
	}
	if err := router.Dispatch(context.Background(), RouteEvent{}); !errors.Is(err, ErrInvalidRouteEvent) {
		t.Fatalf("empty event: %v", err)
	}
	if err := router.Dispatch(context.Background(), RouteEvent{message: request, hasMessage: true}); err != nil {
		t.Fatalf("registered dispatch: %v", err)
	}
	otherID := testOperationID(31)
	other := mustMessage(t, MessageOpenRevisions, &otherID, map[uint64]any{0: uint64(1)})
	if err := router.Dispatch(context.Background(), RouteEvent{message: other, hasMessage: true}); !errors.Is(err, ErrHandlerMissing) {
		t.Fatalf("missing handler: %v", err)
	}
	if err := router.Dispatch(context.Background(), RouteEvent{err: ErrRouterDataFull}); !errors.Is(err, ErrRouterDataFull) {
		t.Fatalf("overflow dispatch: %v", err)
	}

	terminal := mustMessage(t, MessageSessionTerminal, nil, map[uint64]any{0: uint64(1)})
	if disposition, err := router.operations.Observe(DirectionSenderToReceiver, terminal); err != nil || disposition != OperationSessionTerminal {
		t.Fatalf("terminal observation = %v, %v", disposition, err)
	}
	if disposition, err := router.RouteInbound(context.Background(), request); err != nil || disposition != OperationDrop {
		t.Fatalf("post-terminal route = %v, %v", disposition, err)
	}
}

func TestRoleRouterDataOverflowCancelsOnlyItsOperation(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	router, _ := NewRoleRouterWithLimits(RoleReceiver, table, RouterLimits{ControlFrames: 2, DataFrames: 1})
	operationID := testOperationID(40)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := router.AdmitOutbound(request, OutboundOperationPermit{}); err != nil {
		t.Fatal(err)
	}
	first := mustFragmentMessage(t, operationID, 1)
	second := mustFragmentMessage(t, operationID, 2)
	if disposition, err := router.RouteInbound(context.Background(), first); err != nil || disposition != OperationDeliver {
		t.Fatalf("first fragment = %v, %v", disposition, err)
	}
	if disposition, err := router.RouteInbound(context.Background(), second); err != nil || disposition != OperationDrop {
		t.Fatalf("overflow fragment = %v, %v", disposition, err)
	}
	overflow := <-router.control
	if overflow.OperationID() != operationID || !errors.Is(overflow.Error(), ErrRouterDataFull) {
		t.Fatalf("overflow event = %#v", overflow)
	}
	if disposition, err := router.RouteInbound(context.Background(), second); err != nil || disposition != OperationDrop {
		t.Fatalf("late fragment = %v, %v", disposition, err)
	}
	if _, ok := (<-router.data).Message(); !ok {
		t.Fatal("first fragment was not retained")
	}
}

func TestRoleRouterControlOverflowAndRegistrationErrors(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	router, _ := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{ControlFrames: 1, DataFrames: 1})
	firstID, secondID := testOperationID(121), testOperationID(122)
	first := mustMessage(t, MessageListChildren, &firstID, map[uint64]any{0: uint64(1)})
	second := mustMessage(t, MessageOpenRevisions, &secondID, map[uint64]any{0: uint64(1)})
	_, _ = router.RouteInbound(context.Background(), first)
	if _, err := router.RouteInbound(context.Background(), second); !errors.Is(err, ErrRouterControlFull) {
		t.Fatalf("control overflow = %v", err)
	}
	if err := router.RegisterHandler(MessageListChildren, nil); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil handler = %v", err)
	}
	if err := router.RegisterHandler(MessageSessionTerminal, MessageHandlerFunc(func(context.Context, Message) error { return nil })); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("terminal handler = %v", err)
	}
	if err := (*RoleRouter)(nil).Dispatch(context.Background(), RouteEvent{}); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil dispatch = %v", err)
	}
	if _, err := NewRoleRouter(RoleSender, nil); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil operation table = %v", err)
	}

	receiverTable, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTombstones: 1}, nil)
	receiver, _ := NewRoleRouter(RoleReceiver, receiverTable)
	if err := receiver.AcceptOutboundTerminal(); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("receiver terminal = %v", err)
	}
	senderTable, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTombstones: 1}, nil)
	sender, _ := NewRoleRouter(RoleSender, senderTable)
	if err := sender.AcceptOutboundTerminal(); err != nil || !senderTable.Terminated() {
		t.Fatalf("sender terminal = %v", err)
	}
}

func TestRoleRouterPrefersUnrelatedControlWithoutOvertakingOperationData(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 3, MaxTombstones: 3}, nil)
	router, _ := NewRoleRouter(RoleReceiver, table)
	operationID := testOperationID(126)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	_, _ = router.AdmitOutbound(request, OutboundOperationPermit{})
	_, _ = router.RouteInbound(context.Background(), mustFragmentMessage(t, operationID, 1))
	operationError := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
	_, _ = router.RouteInbound(context.Background(), operationError)
	controlOperation := testOperationID(127)
	controlRequest := mustMessage(t, MessageRequestBlocks, &controlOperation, map[uint64]any{0: uint64(1)})
	_, _ = router.AdmitOutbound(controlRequest, OutboundOperationPermit{})
	controlError := mustMessage(t, MessageOperationError, &controlOperation, map[uint64]any{0: uint64(1)})
	_, _ = router.RouteInbound(context.Background(), controlError)
	first, err := router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	message, ok := first.Message()
	firstID, _ := message.OperationID()
	if !ok || message.Kind() != MessageOperationError || firstID != controlOperation {
		t.Fatalf("first event kind = %d, present %v", message.Kind(), ok)
	}
	second, err := router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	message, ok = second.Message()
	if !ok || !message.IsData() {
		t.Fatal("operation data did not follow unrelated control")
	}
	third, err := router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	message, ok = third.Message()
	thirdID, _ := message.OperationID()
	if !ok || message.Kind() != MessageOperationError || thirdID != operationID {
		t.Fatal("same-operation final overtook or escaped its queued data")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := router.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled next = %v", err)
	}
	if _, err := (*RoleRouter)(nil).Next(context.Background()); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil next = %v", err)
	}
}

func TestRoleRouterSustainedControlStillAdvancesData(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 32, MaxTombstones: 32}, nil)
	router, _ := NewRoleRouter(RoleReceiver, table)
	for index := range RouterMaximumControlBurst * 2 {
		operationID := testOperationID(byte(130 + index))
		request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
		if _, err := router.AdmitOutbound(request, OutboundOperationPermit{}); err != nil {
			t.Fatal(err)
		}
		operationError := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
		if _, err := router.RouteInbound(context.Background(), operationError); err != nil {
			t.Fatal(err)
		}
	}
	dataOperation := testOperationID(180)
	request := mustMessage(t, MessageRequestBlocks, &dataOperation, map[uint64]any{0: uint64(1)})
	if _, err := router.AdmitOutbound(request, OutboundOperationPermit{}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.RouteInbound(context.Background(), mustFragmentMessage(t, dataOperation, 1)); err != nil {
		t.Fatal(err)
	}
	dataIndex := -1
	for index := 0; index <= RouterMaximumControlBurst; index++ {
		event, err := router.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		message, ok := event.Message()
		if ok && message.IsData() {
			dataIndex = index
			break
		}
	}
	if dataIndex < 0 || dataIndex > RouterMaximumControlBurst {
		t.Fatalf("data index = %d, control burst limit = %d", dataIndex, RouterMaximumControlBurst)
	}
}

func TestRoleRouterHasOnePrioritySchedulerOwner(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	router, _ := NewRoleRouter(RoleReceiver, table)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := router.Next(ctx)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !router.nextActive.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !router.nextActive.Load() {
		t.Fatal("first router consumer did not start")
	}
	if _, err := router.Next(context.Background()); !errors.Is(err, ErrRouterConsumerBusy) {
		t.Fatalf("second router consumer = %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first router consumer = %v", err)
	}
}

func TestProtocolPumpOwnsRecvAndFailsLocally(t *testing.T) {
	operationID := testOperationID(50)
	message := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	plaintext, _ := EncodeMessage(message)
	channel := newRuntimeChannel(2)
	opener := &stubOpener{opened: OpenedEnvelope{Sequence: 7, Plaintext: plaintext}}
	authenticator := &stubAuthenticator{}
	router := &stubInboundRouter{direction: DirectionReceiverToSender}
	pump, err := NewProtocolPump(channel, opener, authenticator, router)
	if err != nil {
		t.Fatal(err)
	}
	channel.recv <- framechannel.Frame{1}
	close(channel.recv)
	if err := pump.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if channel.recvCalls.Load() != 1 || opener.calls != 1 || authenticator.sequence != 7 || router.message.Kind() != MessageListChildren {
		t.Fatalf("pump calls recv=%d open=%d auth=%d kind=%d", channel.recvCalls.Load(), opener.calls, authenticator.sequence, router.message.Kind())
	}
	if err := pump.Run(context.Background()); !errors.Is(err, ErrPumpReused) {
		t.Fatalf("pump reuse: %v", err)
	}

	for name, configure := range map[string]func(*stubOpener, *stubAuthenticator, *stubInboundRouter){
		"open": func(open *stubOpener, _ *stubAuthenticator, _ *stubInboundRouter) {
			open.err = ErrEnvelopeAuthentication
		},
		"decode": func(open *stubOpener, _ *stubAuthenticator, _ *stubInboundRouter) {
			open.opened.Plaintext = []byte{0xff}
		},
		"authenticate": func(_ *stubOpener, auth *stubAuthenticator, _ *stubInboundRouter) { auth.err = ErrControlSignature },
		"route":        func(_ *stubOpener, _ *stubAuthenticator, route *stubInboundRouter) { route.err = ErrUnknownOperation },
	} {
		t.Run(name, func(t *testing.T) {
			ch := newRuntimeChannel(1)
			open := &stubOpener{opened: OpenedEnvelope{Plaintext: plaintext}}
			auth := &stubAuthenticator{}
			route := &stubInboundRouter{direction: DirectionReceiverToSender}
			configure(open, auth, route)
			candidate, _ := NewProtocolPump(ch, open, auth, route)
			ch.recv <- framechannel.Frame{1}
			close(ch.recv)
			if err := candidate.Run(context.Background()); err == nil {
				t.Fatal("pump accepted a failed stage")
			}
			if !route.terminated {
				t.Fatal("fatal pump rejection did not terminate shared operation state")
			}
		})
	}

	if _, err := (InboundMessageAuthenticatorFunc(nil)).AuthenticateInbound(0, message); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil authenticator function: %v", err)
	}
	if _, err := NewProtocolPump(nil, opener, authenticator, router); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil pump dependency: %v", err)
	}
}

func TestProtocolPumpReturnsAuthenticatedSessionTerminal(t *testing.T) {
	terminal := mustSessionTerminalMessage(t)
	plaintext, _ := EncodeMessage(terminal)
	channel := newRuntimeChannel(1)
	pump, _ := NewProtocolPump(
		channel,
		&stubOpener{opened: OpenedEnvelope{Plaintext: plaintext}},
		&stubAuthenticator{},
		&stubInboundRouter{direction: DirectionSenderToReceiver, disposition: OperationSessionTerminal},
	)
	channel.recv <- framechannel.Frame{1}
	err := pump.Run(context.Background())
	var peerTerminal *PeerSessionTerminalError
	if !errors.As(err, &peerTerminal) || !errors.Is(err, ErrPeerSessionTerminal) || peerTerminal.Message().Kind() != MessageSessionTerminal {
		t.Fatalf("terminal error = %T %v", err, err)
	}
}

func TestProtocolPumpHonorsCancellationAndAuthenticatorFunction(t *testing.T) {
	operationID := testOperationID(123)
	message := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	called := false
	authenticator := InboundMessageAuthenticatorFunc(func(sequence uint64, got Message) (InboundAuthenticationResult, error) {
		called = sequence == 3 && got.Kind() == MessageListChildren
		return InboundAuthenticationResult{}, nil
	})
	if _, err := authenticator.AuthenticateInbound(3, message); err != nil || !called {
		t.Fatalf("authenticator function = called %v err %v", called, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	channel := newRuntimeChannel(0)
	pump, _ := NewProtocolPump(channel, &stubOpener{}, &stubAuthenticator{}, &stubInboundRouter{})
	if err := pump.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pump = %v", err)
	}
	terminal := &PeerSessionTerminalError{message: mustSessionTerminalMessage(t)}
	if terminal.Error() != ErrPeerSessionTerminal.Error() || !errors.Is(terminal.Unwrap(), ErrPeerSessionTerminal) {
		t.Fatal("peer terminal error classification changed")
	}
}

type stubOpener struct {
	opened OpenedEnvelope
	err    error
	calls  int
}

func (o *stubOpener) Open([]byte) (OpenedEnvelope, error) {
	o.calls++
	return o.opened, o.err
}

type stubAuthenticator struct {
	sequence uint64
	message  Message
	result   InboundAuthenticationResult
	err      error
}

func (a *stubAuthenticator) AuthenticateInbound(
	sequence uint64,
	message Message,
) (InboundAuthenticationResult, error) {
	a.sequence, a.message = sequence, message
	return a.result, a.err
}

type stubInboundRouter struct {
	direction   Direction
	message     Message
	disposition OperationDisposition
	err         error
	terminalErr error
	terminated  bool
	violation   AuthenticatedOperationViolation
	bound       bool
}

func (r *stubInboundRouter) RouteInbound(_ context.Context, message Message) (OperationDisposition, error) {
	r.message = message
	return r.disposition, r.err
}

func (r *stubInboundRouter) RouteAuthenticatedOperationViolation(
	_ context.Context,
	message Message,
	violation AuthenticatedOperationViolation,
) (bool, error) {
	r.message = message
	r.violation = violation
	return r.bound, r.err
}

func (r *stubInboundRouter) InboundDirection() Direction { return r.direction }

func (r *stubInboundRouter) TerminateLocal() error {
	r.terminated = true
	return r.terminalErr
}

type runtimeChannel struct {
	recv      chan framechannel.Frame
	recvCalls atomic.Int32
	mu        sync.Mutex
	sent      []framechannel.Frame
	terminal  []framechannel.Frame
}

func newRuntimeChannel(capacity int) *runtimeChannel {
	return &runtimeChannel{recv: make(chan framechannel.Frame, capacity)}
}

func (c *runtimeChannel) Send(_ context.Context, frame framechannel.Frame) error {
	c.mu.Lock()
	c.sent = append(c.sent, append(framechannel.Frame(nil), frame...))
	c.mu.Unlock()
	return nil
}

func (c *runtimeChannel) SendTerminal(_ context.Context, frame framechannel.Frame) error {
	c.mu.Lock()
	c.terminal = append(c.terminal, append(framechannel.Frame(nil), frame...))
	c.mu.Unlock()
	return nil
}

func (c *runtimeChannel) Recv() <-chan framechannel.Frame {
	c.recvCalls.Add(1)
	return c.recv
}

func (*runtimeChannel) State() framechannel.ChannelState { return framechannel.Open }
func (*runtimeChannel) Close() error                     { return nil }

func mustBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := EncodeBody(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustMessage(t *testing.T, kind MessageKind, operationID *OperationID, value any) Message {
	t.Helper()
	message, err := NewMessage(kind, operationID, mustBody(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func mustFragmentMessage(t *testing.T, operationID OperationID, marker byte) Message {
	t.Helper()
	plaintext := make([]byte, fragmentRoutingHeaderBytes+1)
	plaintext[0], plaintext[1] = 1, byte(MessageBlockFragment)
	copy(plaintext[4:fragmentRoutingHeaderBytes], operationID[:])
	plaintext[fragmentRoutingHeaderBytes] = marker
	message, err := DecodeMessage(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func mustSessionTerminalMessage(t *testing.T) Message {
	t.Helper()
	semantic, err := EncodeSessionTerminal(SessionTerminal{
		Code: SessionTerminalCodeLast, Message: "Sender stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := encodeSignedControlWrapper(semantic, make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	message, err := NewMessage(MessageSessionTerminal, nil, signed)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func testOperationID(first byte) OperationID {
	var id OperationID
	for index := range id {
		id[index] = first + byte(index)
	}
	return id
}

func mutateBytes(source []byte, mutate func([]byte)) []byte {
	result := append([]byte(nil), source...)
	mutate(result)
	return result
}

func (event RouteEvent) String() string {
	return fmt.Sprintf("message=%v operation=%x error=%v", event.hasMessage, event.operationID, event.err)
}
