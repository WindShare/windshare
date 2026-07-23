package protocolsession

import (
	"bytes"
	"context"
	"errors"
	"testing"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

func TestSendReceiptPreflightDropDoesNotReserveAnUnsentOperation(t *testing.T) {
	operations, err := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRoleRouter(RoleReceiver, operations)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(210)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	_, vectorKey, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	receiverKey, err := TrafficKeyFromBytes(vectorKey.Bytes(), DirectionReceiverToSender)
	if err != nil {
		t.Fatal(err)
	}
	binding.Direction = DirectionReceiverToSender
	failingSealer, err := NewEnvelopeSealer(receiverKey, binding, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	failingWriter, err := NewSessionWriter(newRuntimeChannel(0), failingSealer, router)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := failingWriter.TryControl(request)
	if err != nil {
		t.Fatal(err)
	}
	if runErr := failingWriter.Run(context.Background()); !errors.Is(runErr, ErrNonceSource) {
		t.Fatalf("nonce preflight error = %v", runErr)
	}
	completion := receipt.Await(context.Background())
	if !completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeDropped ||
		!completion.RetryableAcrossLane || !completion.Replay.IsZero() || !errors.Is(completion.Err, ErrNonceSource) {
		t.Fatalf("preflight completion = %+v", completion)
	}
	if operations.ActiveCount() != 0 || operations.TombstoneCount() != 0 {
		t.Fatalf("unsent request changed lifecycle: active=%d tombstones=%d", operations.ActiveCount(), operations.TombstoneCount())
	}

	validSealer, err := NewEnvelopeSealer(receiverKey, binding, bytes.NewReader(make([]byte, EnvelopeNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	channel := newRuntimeChannel(0)
	retryWriter, err := NewSessionWriter(channel, validSealer, router)
	if err != nil {
		t.Fatal(err)
	}
	retryReceipt, err := retryWriter.TryControl(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- retryWriter.Run(ctx) }()
	if outcome, waitErr := retryReceipt.Wait(context.Background()); outcome != SendOutcomeDelivered || waitErr != nil {
		t.Fatalf("retry receipt = %d, %v", outcome, waitErr)
	}
	cancel()
	<-done
	if operations.ActiveCount() != 1 {
		t.Fatalf("delivered retry did not register its operation: %d", operations.ActiveCount())
	}
}

func TestSendReceiptMakesCancelBeforeSendAndSendBeforeCancelUnambiguous(t *testing.T) {
	t.Run("cancel before send", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		sealer := &passthroughSealer{next: 17}
		_, router, writer, receipt, operationID := queuedOpenResult(t, channel, sealer)
		cancelMessage := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
		if _, err := router.RouteInbound(context.Background(), cancelMessage); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(ctx) }()
		outcome, err := receipt.Wait(context.Background())
		if err != nil || outcome != SendOutcomeDropped {
			t.Fatalf("cancel-before-send outcome=%d err=%v", outcome, err)
		}
		if sealer.next != 17 {
			t.Fatalf("definitively dropped control consumed sequence %d", sealer.next)
		}
		channel.mu.Lock()
		sent := len(channel.sent)
		channel.mu.Unlock()
		if sent != 0 {
			t.Fatalf("definitively dropped control reached transport %d times", sent)
		}
		cancel()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop=%v", err)
		}
	})

	t.Run("send wins cancel", func(t *testing.T) {
		channel := newGatedWriterChannel()
		_, router, writer, receipt, operationID := queuedOpenResult(t, channel, &passthroughSealer{next: 23})
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(ctx) }()
		<-channel.firstStarted

		cancelMessage := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
		if disposition, err := router.RouteInbound(context.Background(), cancelMessage); err != nil || disposition != OperationDrop {
			t.Fatalf("late cancel did not yield to the admitted final: disposition=%d err=%v", disposition, err)
		}
		close(channel.releaseFirst)
		outcome, err := receipt.Wait(context.Background())
		if err != nil || outcome != SendOutcomeDelivered {
			t.Fatalf("send-before-cancel outcome=%d err=%v", outcome, err)
		}
		if len(channel.frames()) != 1 {
			t.Fatal("delivered control did not reach transport exactly once")
		}
		cancel()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop=%v", err)
		}
	})
}

func TestSendReceiptKeepsTransportFailureAndCallerTimeoutUncertain(t *testing.T) {
	t.Run("seal failure after admission", func(t *testing.T) {
		sealErr := errors.New("seal failed after operation admission")
		_, _, writer, receipt, _ := queuedOpenResult(
			t, newRuntimeChannel(0), &failingSealer{sealErr: sealErr},
		)
		if err := writer.Run(context.Background()); !errors.Is(err, sealErr) {
			t.Fatalf("writer error=%v", err)
		}
		completion := receipt.Await(context.Background())
		if !completion.Settled || !completion.Admitted || !completion.RetryableAcrossLane || completion.Replay.IsZero() ||
			completion.Outcome != SendOutcomeDropped || !errors.Is(completion.Err, sealErr) {
			t.Fatalf("post-admission seal completion=%+v", completion)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		transportErr := errors.New("transport acceptance is uncertain")
		_, _, writer, receipt, _ := queuedOpenResult(t, &failingChannel{sendErr: transportErr}, &passthroughSealer{})
		if err := writer.Run(context.Background()); !errors.Is(err, transportErr) {
			t.Fatalf("writer error=%v", err)
		}
		completion := receipt.Await(context.Background())
		if !completion.Settled || !completion.Admitted || !completion.RetryableAcrossLane || completion.Replay.IsZero() ||
			completion.TransportDisposition != framechannel.SendAccepted || !errors.Is(completion.Err, transportErr) ||
			completion.Outcome != SendOutcomeUnknown {
			t.Fatalf("transport failure completion=%+v", completion)
		}
	})

	t.Run("classified pre-transport failures", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			sendErr func(error) error
			retired bool
		}{
			{name: "local rejection", sendErr: framechannel.RejectSend},
			{name: "channel retirement", sendErr: framechannel.RetireSend, retired: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				transportErr := errors.New("transport did not acquire frame")
				_, _, writer, receipt, _ := queuedOpenResult(
					t,
					&failingChannel{sendErr: test.sendErr(transportErr)},
					&passthroughSealer{},
				)
				if err := writer.Run(context.Background()); !errors.Is(err, transportErr) {
					t.Fatalf("writer error=%v", err)
				}
				completion := receipt.Await(context.Background())
				if !completion.Settled || !completion.Admitted || !completion.RetryableAcrossLane ||
					completion.Replay.IsZero() || completion.Outcome != SendOutcomeDropped ||
					(completion.TransportDisposition == framechannel.SendRetired) != test.retired ||
					!errors.Is(completion.Err, transportErr) {
					t.Fatalf("classified completion=%+v", completion)
				}
			})
		}
	})

	t.Run("caller cancellation retracts pending item", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		_, router, writer, receipt, _ := queuedOpenResult(t, channel, &passthroughSealer{})
		waitContext, stopWaiting := context.WithCancel(context.Background())
		stopWaiting()
		completion := receipt.Await(waitContext)
		if !completion.Settled || completion.Admitted || completion.RetryableAcrossLane || !completion.Replay.IsZero() ||
			!errors.Is(completion.Err, context.Canceled) || completion.Outcome != SendOutcomeDropped {
			t.Fatalf("caller timeout completion=%+v", completion)
		}
		secondID := testOperationID(213)
		request := mustMessage(t, MessageOpenRevisions, &secondID, map[uint64]any{0: uint64(1)})
		admission, err := router.operations.ObserveInbound(DirectionReceiverToSender, request)
		if err != nil {
			t.Fatal(err)
		}
		second, err := writer.TryAuthorizedSenderControl(
			mustPreparedControl(t, MessageOpenResults, &secondID), admission.Outbound,
		)
		if err != nil {
			t.Fatal(err)
		}

		runContext, stopWriter := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(runContext) }()
		outcome, err := second.Wait(context.Background())
		if err != nil || outcome != SendOutcomeDelivered {
			t.Fatalf("sentinel completion outcome=%d err=%v", outcome, err)
		}
		channel.mu.Lock()
		sent := len(channel.sent)
		channel.mu.Unlock()
		if sent != 1 {
			t.Fatalf("retracted item reached transport: frames=%d", sent)
		}
		stopWriter()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop=%v", err)
		}
	})

	t.Run("caller cancellation after admission remains uncertain", func(t *testing.T) {
		channel := newGatedWriterChannel()
		_, _, writer, receipt, _ := queuedOpenResult(t, channel, &passthroughSealer{})
		runContext, stopWriter := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(runContext) }()
		<-channel.firstStarted
		waitContext, stopWaiting := context.WithCancel(context.Background())
		stopWaiting()
		completion := receipt.Await(waitContext)
		if completion.Settled || !completion.Admitted || completion.RetryableAcrossLane ||
			completion.Replay.IsZero() || completion.Outcome != SendOutcomeUnknown ||
			!errors.Is(completion.Err, context.Canceled) {
			t.Fatalf("post-admission cancellation=%+v", completion)
		}
		close(channel.releaseFirst)
		if outcome, err := receipt.Wait(context.Background()); outcome != SendOutcomeDelivered || err != nil {
			t.Fatalf("physical completion=%d, %v", outcome, err)
		}
		stopWriter()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop=%v", err)
		}
	})

	t.Run("receiver request hides replay until settled physical unknown", func(t *testing.T) {
		operations, err := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		if err != nil {
			t.Fatal(err)
		}
		router, err := NewRoleRouter(RoleReceiver, operations)
		if err != nil {
			t.Fatal(err)
		}
		transportErr := errors.New("receiver request acceptance is uncertain")
		channel := newSettlementGateChannel(transportErr)
		writer, err := NewSessionWriter(channel, &passthroughSealer{}, router)
		if err != nil {
			t.Fatal(err)
		}
		operationID := testOperationID(215)
		request := mustMessage(t, MessagePeerOffer, &operationID, map[uint64]any{0: uint64(1)})
		receipt, err := writer.TryControl(request)
		if err != nil {
			t.Fatal(err)
		}
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(context.Background()) }()
		released, joined := false, false
		defer func() {
			if !released {
				close(channel.release)
			}
			if !joined {
				<-runDone
			}
		}()
		<-channel.entered

		waitContext, stopWaiting := context.WithCancel(context.Background())
		stopWaiting()
		unsettled := receipt.Await(waitContext)
		if unsettled.Settled || !unsettled.Admitted || unsettled.Outcome != SendOutcomeUnknown ||
			!unsettled.Replay.IsZero() || unsettled.Generation.IsZero() || unsettled.Operation.IsZero() ||
			!errors.Is(unsettled.Err, context.Canceled) {
			t.Fatalf("unsettled receiver request completion=%+v", unsettled)
		}

		close(channel.release)
		released = true
		settled := receipt.Await(context.Background())
		if !settled.Settled || !settled.Admitted || settled.Outcome != SendOutcomeUnknown ||
			settled.Replay.IsZero() || !settled.Generation.Same(unsettled.Generation) ||
			!settled.Operation.Generation().Same(unsettled.Operation.Generation()) ||
			!errors.Is(settled.Err, transportErr) {
			t.Fatalf("settled receiver request completion=%+v", settled)
		}
		if runErr := <-runDone; !errors.Is(runErr, transportErr) {
			t.Fatalf("receiver request writer error=%v", runErr)
		}
		joined = true
	})

	t.Run("session close before send", func(t *testing.T) {
		_, _, writer, receipt, _ := queuedOpenResult(t, newRuntimeChannel(0), &passthroughSealer{})
		closed, closeSession := context.WithCancel(context.Background())
		closeSession()
		if err := writer.Run(closed); !errors.Is(err, context.Canceled) {
			t.Fatalf("writer close=%v", err)
		}
		outcome, err := receipt.Wait(context.Background())
		if !errors.Is(err, ErrWriterStopped) || outcome != SendOutcomeDropped {
			t.Fatalf("session-close outcome=%d err=%v", outcome, err)
		}
	})
}

func TestSendReceiptCompletionWinsCallerCancellationRace(t *testing.T) {
	result := newDeliveryResult()
	result.complete(SendOutcomeDelivered, OutboundReplayPermit{}, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	completion := result.receipt().Await(ctx)
	if !completion.Settled || completion.Outcome != SendOutcomeDelivered || completion.Err != nil {
		t.Fatalf("completed receipt lost cancellation race: %+v", completion)
	}
}

func TestSendReceiptSealingSnapshotWithholdsUncommittedReplay(t *testing.T) {
	operationID := testOperationID(216)
	candidate := mustMessage(t, MessagePeerCandidate, &operationID, map[uint64]any{0: "candidate"})
	replay := testReplayPermit(candidate, DirectionSenderToReceiver)
	result := newDeliveryResult()
	if !result.reserveBeforeQueue(OutboundAdmission{Replay: replay}) {
		t.Fatal("reserve delivery")
	}
	claimed, policyPrepared, alreadyAdmitted := result.claim()
	if !claimed || !policyPrepared || alreadyAdmitted || !result.beginReservationSeal() {
		t.Fatalf(
			"sealing transition: claimed=%v prepared=%v admitted=%v",
			claimed, policyPrepared, alreadyAdmitted,
		)
	}
	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	unsettled := result.receipt().Await(waitContext)
	if unsettled.Settled || unsettled.Admitted || unsettled.Outcome != SendOutcomeUnknown ||
		!unsettled.Replay.IsZero() || !errors.Is(unsettled.Err, context.Canceled) {
		t.Fatalf("sealing snapshot = %+v", unsettled)
	}
	if err := result.commitReservationSeal(); err != nil {
		t.Fatal(err)
	}
	result.complete(SendOutcomeDelivered, replay, false, nil)
	settled := result.receipt().Await(context.Background())
	if !settled.Settled || !settled.Admitted || settled.Outcome != SendOutcomeDelivered ||
		settled.Replay.IsZero() || settled.Err != nil {
		t.Fatalf("committed completion = %+v", settled)
	}
}

type missingReplayPermitPolicy struct{}

func (missingReplayPermitPolicy) AdmitOutbound(Message, OutboundOperationPermit) (OutboundAdmission, error) {
	return OutboundAdmission{Disposition: OperationDeliver}, nil
}
func (missingReplayPermitPolicy) AcceptOutboundReplay(
	Message,
	OutboundReplayPermit,
) (OutboundAdmission, error) {
	return OutboundAdmission{Disposition: OperationDeliver}, nil
}
func (missingReplayPermitPolicy) AcceptOutboundTerminal() error { return nil }
func (missingReplayPermitPolicy) OutboundDirection() Direction  { return DirectionSenderToReceiver }

func TestSenderWriterRejectsDeliveredAdmissionWithoutReplayPermit(t *testing.T) {
	channel := newRuntimeChannel(0)
	writer, err := NewSessionWriter(channel, &passthroughSealer{}, missingReplayPermitPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(212)
	receipt, err := writer.TrySenderControl(mustPreparedControl(t, MessageOpenResults, &operationID))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Run(context.Background()); !errors.Is(err, ErrOutboundReplayPermit) {
		t.Fatalf("writer error=%v", err)
	}
	completion := receipt.Await(context.Background())
	if !completion.Admitted || completion.Outcome != SendOutcomeDropped || !errors.Is(completion.Err, ErrOutboundReplayPermit) {
		t.Fatalf("completion=%+v", completion)
	}
	channel.mu.Lock()
	sent := len(channel.sent)
	channel.mu.Unlock()
	if sent != 0 {
		t.Fatal("permit-less response reached the transport")
	}
}

func queuedOpenResult(
	t *testing.T,
	channel FrameChannel,
	sealer OutboundEnvelopeSealer,
) (*OperationTable, *RoleRouter, *SessionWriter, SendReceipt, OperationID) {
	t.Helper()
	operations, err := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRoleRouter(RoleSender, operations)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(211)
	request := mustMessage(t, MessageOpenRevisions, &operationID, map[uint64]any{0: uint64(1)})
	admission, err := operations.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewSessionWriter(channel, sealer, router)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := writer.TryAuthorizedSenderControl(
		mustPreparedControl(t, MessageOpenResults, &operationID), admission.Outbound,
	)
	if err != nil {
		t.Fatal(err)
	}
	return operations, router, writer, receipt, operationID
}
