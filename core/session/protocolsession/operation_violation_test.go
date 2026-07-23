package protocolsession

import (
	"context"
	"errors"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

func TestAuthenticatedOperationViolationSurvivesTombstoneAndRegistrationRace(t *testing.T) {
	table, err := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(0xd1)
	request := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
	admission, err := table.AdmitOutbound(DirectionReceiverToSender, request, OutboundOperationPermit{})
	if err != nil || admission.Generation.IsZero() {
		t.Fatalf("admit peer request: generation=%+v err=%v", admission.Generation, err)
	}
	if err := table.CancelGeneration(admission.Generation); err != nil {
		t.Fatal(err)
	}

	failure := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
	violation := AuthenticatedOperationViolation{code: AuthenticatedOperationViolationMalformedFailure}
	if bound, err := table.RecordAuthenticatedOperationViolation(failure, violation); err != nil || !bound {
		t.Fatalf("record tombstoned violation: bound=%t err=%v", bound, err)
	}
	var observed []AuthenticatedOperationViolationCode
	if err := admission.Generation.RegisterAuthenticatedOperationViolationHandler(
		func(got AuthenticatedOperationViolation) { observed = append(observed, got.Code()) },
	); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0] != AuthenticatedOperationViolationMalformedFailure {
		t.Fatalf("pending violation = %v", observed)
	}
	if err := admission.Generation.RegisterAuthenticatedOperationViolationHandler(func(AuthenticatedOperationViolation) {}); !errors.Is(err, ErrAuthenticatedOperationObserver) {
		t.Fatalf("second observer registration = %v", err)
	}

	unmatchedID := testOperationID(0xd2)
	unmatched := mustMessage(t, MessageOperationError, &unmatchedID, map[uint64]any{0: uint64(1)})
	if bound, err := table.RecordAuthenticatedOperationViolation(unmatched, violation); err != nil || bound {
		t.Fatalf("unmatched violation: bound=%t err=%v", bound, err)
	}
}

func TestAuthenticatedOperationViolationObserverCannotCrossGeneration(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	table, err := NewOperationTable(
		OperationLimits{MaxActive: 4, MaxTombstones: 4},
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(0xd5)
	request := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
	first, err := table.AdmitOutbound(DirectionReceiverToSender, request, OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.CancelGeneration(first.Generation); err != nil {
		t.Fatal(err)
	}
	first.pin.release()
	now = now.Add(OperationTombstoneLifetime + time.Nanosecond)
	second, err := table.AdmitOutbound(DirectionReceiverToSender, request, OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation.Same(second.Generation) {
		t.Fatal("same wire identity reused an old exact generation")
	}
	defer second.pin.release()
	if err := first.Generation.RegisterAuthenticatedOperationViolationHandler(
		func(AuthenticatedOperationViolation) {},
	); !errors.Is(err, ErrAuthenticatedOperationObserver) {
		t.Fatalf("stale generation observer registration = %v", err)
	}
	observed := make(chan AuthenticatedOperationViolationCode, 1)
	if err := second.Generation.RegisterAuthenticatedOperationViolationHandler(
		func(violation AuthenticatedOperationViolation) { observed <- violation.Code() },
	); err != nil {
		t.Fatal(err)
	}
	failure := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
	if bound, err := table.RecordAuthenticatedOperationViolation(
		failure,
		AuthenticatedOperationViolation{code: AuthenticatedOperationViolationMalformedFailure},
	); err != nil || !bound {
		t.Fatalf("new-generation violation: bound=%t err=%v", bound, err)
	}
	select {
	case code := <-observed:
		if code != AuthenticatedOperationViolationMalformedFailure {
			t.Fatalf("new-generation violation code = %d", code)
		}
	default:
		t.Fatal("new exact generation did not receive its violation")
	}
}

func TestOperationTablePublishesConflictingPeerAnswerStructurally(t *testing.T) {
	table, err := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(0xd3)
	request := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
	admission, err := table.AdmitOutbound(DirectionReceiverToSender, request, OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan AuthenticatedOperationViolationCode, 1)
	if err := admission.Generation.RegisterAuthenticatedOperationViolationHandler(
		func(violation AuthenticatedOperationViolation) { observed <- violation.Code() },
	); err != nil {
		t.Fatal(err)
	}
	first := mustMessage(t, MessagePeerAnswer, &operationID, map[uint64]any{0: uint64(2)})
	if disposition, err := table.ObserveInbound(DirectionSenderToReceiver, first); err != nil || disposition.Disposition != OperationDeliver {
		t.Fatalf("first answer: disposition=%d err=%v", disposition.Disposition, err)
	}
	conflicting := mustMessage(t, MessagePeerAnswer, &operationID, map[uint64]any{0: uint64(3)})
	if disposition, err := table.ObserveInbound(DirectionSenderToReceiver, conflicting); disposition.Disposition != OperationDrop || !errors.Is(err, ErrOperationIDReused) {
		t.Fatalf("conflicting answer: disposition=%d err=%v", disposition.Disposition, err)
	}
	select {
	case code := <-observed:
		if code != AuthenticatedOperationViolationConflictingPeerAnswer {
			t.Fatalf("violation code = %d", code)
		}
	default:
		t.Fatal("conflicting authenticated answer did not notify exact operation authority")
	}
}

func TestWriterRegistersViolationObserverBeforeRequestExposure(t *testing.T) {
	table, err := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRoleRouter(RoleReceiver, table)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(0xd4)
	failure := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
	observed := make(chan AuthenticatedOperationViolationCode, 1)
	recorded := make(chan error, 1)
	channel := &violationOnSendChannel{
		receive: make(chan framechannel.Frame),
		onSend: func() {
			bound, recordErr := table.RecordAuthenticatedOperationViolation(
				failure,
				AuthenticatedOperationViolation{code: AuthenticatedOperationViolationMalformedFailure},
			)
			if recordErr == nil && !bound {
				recordErr = errors.New("request generation was not visible at transport exposure")
			}
			recorded <- recordErr
		},
	}
	writer, err := NewSessionWriter(channel, &passthroughSealer{}, router)
	if err != nil {
		t.Fatal(err)
	}
	request := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
	receipt, err := writer.TryControlObservingAuthenticatedViolations(
		request,
		func(violation AuthenticatedOperationViolation) { observed <- violation.Code() },
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Run(ctx) }()
	if outcome, err := receipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
		t.Fatalf("offer delivery: outcome=%d err=%v", outcome, err)
	}
	if err := <-recorded; err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-observed:
		if code != AuthenticatedOperationViolationMalformedFailure {
			t.Fatalf("pre-send violation code = %d", code)
		}
	default:
		t.Fatal("transport exposure preceded exact-generation observer registration")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("writer stop = %v", err)
	}
}

type violationOnSendChannel struct {
	receive <-chan framechannel.Frame
	onSend  func()
}

func (channel *violationOnSendChannel) Send(context.Context, framechannel.Frame) error {
	channel.onSend()
	return nil
}

func (*violationOnSendChannel) SendTerminal(context.Context, framechannel.Frame) error { return nil }
func (channel *violationOnSendChannel) Recv() <-chan framechannel.Frame                { return channel.receive }
func (*violationOnSendChannel) State() framechannel.ChannelState                       { return framechannel.Open }
func (*violationOnSendChannel) Close() error                                           { return nil }
