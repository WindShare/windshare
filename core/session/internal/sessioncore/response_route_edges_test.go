package sessioncore

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type nonComparableOperationState struct {
	// Include a slice field so the value type is uncomparable in Go; interface
	// equality against *operationTable must still return false without panicking.
	uncomparable []byte
}

func (nonComparableOperationState) Active(protocolsession.OperationID) bool  { return true }
func (nonComparableOperationState) Current(protocolsession.OperationID) bool { return true }
func (nonComparableOperationState) RequestKind(protocolsession.OperationID) (protocolsession.MessageKind, bool) {
	return protocolsession.MessageRequestBlocks, true
}
func (nonComparableOperationState) PeerAttemptBinding(protocolsession.OperationID) (protocolsession.PeerAttemptBinding, bool) {
	return protocolsession.PeerAttemptBinding{}, false
}

func TestResponseWithoutLaneAndMalformedFailureReleaseRoute(t *testing.T) {
	sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	identity := sender.initial
	outbound := NewResponses(sender, ResponseConfig{})
	fragments, err := contentflow.FragmentRecord(protocolsession.OperationID(77), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	sender.lanes.detach(identity)
	if err := outbound.SendFragment(context.Background(), protocolsession.Operation{}, fragments[0]); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("fragment without operation authority error = %v", err)
	}
	invalidFailureID := protocolsession.OperationID(76)
	request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &invalidFailureID, []byte{0xa0})
	admission, err := sender.operations.ObserveInbound(protocolsession.DirectionReceiverToSender, identity, request)
	if err != nil || admission.op.IsZero() {
		t.Fatalf("admit request: %v", err)
	}
	if err := outbound.SendOperationError(context.Background(), admission.op, contentflow.OperationFailure{}); err == nil {
		t.Fatal("invalid operation failure was sent")
	}
	if admission.op.IsActive() || sender.operations.ActiveCount() != 0 || sender.operations.TombstoneCount() != 1 {
		t.Fatalf("invalid operation failure retained active operation: active=%d tombstones=%d",
			sender.operations.ActiveCount(), sender.operations.TombstoneCount())
	}
}

func TestResponsesRejectForeignSessionAndSyntheticOperationHandles(t *testing.T) {
	sessionA, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	sessionB, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorderB := newProtocolTraceRecorder(sessionB)

	localB, peerB := newMemoryChannelPair()
	t.Cleanup(func() { _ = peerB.Close() })
	laneIdentityB := session.LaneIdentity{ID: sessionB.initial.ID + 1, Epoch: 1}
	laneB, err := sessionB.lanes.add(laneIdentityB, localB, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	sessionB.lanes.detach(sessionB.initial)
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- laneB.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()

	sharedID := protocolsession.OperationID(1)
	blockReq, err := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &sharedID, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	opA := admitTestSenderOperation(t, sessionA, sessionA.initial, blockReq)
	opB := admitTestSenderOperation(t, sessionB, laneIdentityB, blockReq)

	mismatchedKindID := protocolsession.OperationID(2)
	openReq, err := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &mismatchedKindID, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	listReq, err := protocolsession.NewMessage(protocolsession.MessageListChildren, &mismatchedKindID, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	opA2 := admitTestSenderOperation(t, sessionA, sessionA.initial, openReq)
	opB2 := admitTestSenderOperation(t, sessionB, laneIdentityB, listReq)

	outboundB := NewResponses(sessionB, ResponseConfig{})
	completeBody, err := contentflow.EncodeOperationComplete(1)
	if err != nil {
		t.Fatal(err)
	}
	fragmentsID1, err := contentflow.FragmentRecord(sharedID, []byte{0x01, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	otherID := protocolsession.OperationID(99)
	fragmentsOtherID, err := contentflow.FragmentRecord(otherID, []byte{0x03})
	if err != nil {
		t.Fatal(err)
	}
	validFailure := contentflow.OperationFailure{
		Scope:   contentflow.BlockErrorScope,
		Code:    contentflow.BlockCodeOutOfRange,
		Message: "foreign failure",
	}

	assertRejected := func(name string, evidence protocolsession.SendEvidence, sendErr error) {
		t.Helper()
		if evidence != protocolsession.SendNotSent ||
			!errors.Is(sendErr, session.ErrOperationMissing) ||
			!protocolsession.IsNotSent(sendErr) ||
			errors.Is(sendErr, protocolsession.ErrOperationSuppressed) {
			t.Fatalf("%s = (evidence=%d, err=%v), want (SendNotSent, MarkNotSent(ErrOperationMissing))",
				name, evidence, sendErr)
		}
	}

	// Same ID and compatible RequestKind must still be rejected across sessions.
	evidence, err := outboundB.SendControl(context.Background(), opA, protocolsession.MessageOperationComplete, completeBody)
	assertRejected("foreign SendControl matching kind", evidence, err)

	// Same ID with mismatched RequestKind must not cancel sessionB's operation.
	evidence, err = outboundB.SendControl(context.Background(), opA2, protocolsession.MessageOpenResults, []byte{0xa0})
	assertRejected("foreign SendControl mismatched kind", evidence, err)

	// Preparation failures with a foreign handle must not cancel sessionB's operation.
	evidence, err = outboundB.SendControl(context.Background(), opA, protocolsession.MessageKind(0xff), nil)
	assertRejected("foreign SendControl invalid kind", evidence, err)

	err = outboundB.SendFragment(context.Background(), opA, fragmentsID1[0])
	assertRejected("foreign SendFragment matching ID", protocolsession.SendNotSent, err)

	err = outboundB.SendFragment(context.Background(), opA, fragmentsOtherID[0])
	assertRejected("foreign SendFragment mismatched ID", protocolsession.SendNotSent, err)

	err = outboundB.SendOperationError(context.Background(), opA, validFailure)
	assertRejected("foreign SendOperationError valid failure", protocolsession.SendNotSent, err)

	err = outboundB.SendOperationError(context.Background(), opA, contentflow.OperationFailure{})
	assertRejected("foreign SendOperationError invalid failure", protocolsession.SendNotSent, err)

	// Non-comparable synthetic OperationState must fail closed without panicking.
	syntheticOp := protocolsession.NewOperation(sharedID, nonComparableOperationState{uncomparable: []byte{1}})
	evidence, err = outboundB.SendControl(context.Background(), syntheticOp, protocolsession.MessageOperationComplete, completeBody)
	assertRejected("synthetic non-comparable SendControl", evidence, err)

	// Direct operationTable admission boundary must also enforce table ownership.
	completeMsg, err := protocolsession.NewMessage(protocolsession.MessageOperationComplete, &sharedID, completeBody)
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := sessionB.operations.BeginOutboundResponse(opA, completeMsg); lease != nil ||
		!errors.Is(err, session.ErrOperationMissing) || !protocolsession.IsNotSent(err) {
		t.Fatalf("BeginOutboundResponse(foreign) = (%v, %v), want (nil, MarkNotSent(ErrOperationMissing))", lease, err)
	}

	// Nil/uninitialized Responses must fail closed without panicking.
	evidence, err = (&Responses{}).SendControl(context.Background(), opB, protocolsession.MessageOperationComplete, completeBody)
	assertRejected("uninitialized Responses SendControl", evidence, err)

	if !opB.IsActive() || !opB2.IsActive() || sessionB.operations.ActiveCount() != 2 || sessionB.operations.TombstoneCount() != 0 {
		t.Fatalf("foreign responses mutated sessionB state: opB=%v opB2=%v active=%d tombstones=%d",
			opB.IsActive(), opB2.IsActive(), sessionB.operations.ActiveCount(), sessionB.operations.TombstoneCount())
	}
	select {
	case frame := <-peerB.Recv():
		t.Fatalf("foreign response leaked wire frame to sessionB: %d bytes", len(frame))
	default:
	}

	facts := recorderB.facts()
	if len(facts) != 8 {
		t.Fatalf("sessionB observed %d rejected responses, want 8", len(facts))
	}
	for index, fact := range facts {
		observed, ok := fact.(sessionobs.ResponseSendObserved)
		if !ok {
			t.Fatalf("fact[%d] = %T, want ResponseSendObserved", index, fact)
		}
		correlation := observed.Correlation()
		if correlation.OperationID != 0 || correlation.RequestKind != 0 ||
			observed.Status() != sessionobs.SendStatusNotSent || observed.Attempts() != 0 {
			t.Fatalf("fact[%d] leaked foreign context or wrong status: corr=%+v status=%v attempts=%d",
				index, correlation, observed.Status(), observed.Attempts())
		}
	}

	// Positive control: sessionB's own handle still completes cleanly without affecting sessionA.
	evidence, err = outboundB.SendControl(context.Background(), opB, protocolsession.MessageOperationComplete, completeBody)
	if evidence != protocolsession.SendDelivered || err != nil {
		t.Fatalf("own SendControl = (evidence=%d, err=%v), want (SendDelivered, nil)", evidence, err)
	}
	select {
	case frame := <-peerB.Recv():
		if len(frame) == 0 {
			t.Fatal("sessionB delivered empty wire frame")
		}
	default:
		t.Fatal("sessionB did not deliver frame for its own operation")
	}
	if opB.IsActive() || !opA.IsActive() || sessionB.operations.ActiveCount() != 1 || sessionB.operations.TombstoneCount() != 1 {
		t.Fatalf("post-commit isolation broken: opB=%v opA=%v activeB=%d tombstonesB=%d",
			opB.IsActive(), opA.IsActive(), sessionB.operations.ActiveCount(), sessionB.operations.TombstoneCount())
	}
}
