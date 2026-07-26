package protocolsession

import (
	"context"
	"errors"
	"testing"
)

type writerAdmissionPolicy struct {
	direction       Direction
	admission       OutboundAdmission
	replayAdmission OutboundAdmission
	admitErr        error
	replayErr       error
	terminalErr     error
	admitCalls      int
	replayCalls     int
}

func (policy *writerAdmissionPolicy) AdmitOutbound(
	Message,
	OutboundOperationPermit,
) (OutboundAdmission, error) {
	policy.admitCalls++
	return policy.admission, policy.admitErr
}

func (policy *writerAdmissionPolicy) AcceptOutboundReplay(
	Message,
	OutboundReplayPermit,
) (OutboundAdmission, error) {
	policy.replayCalls++
	return policy.replayAdmission, policy.replayErr
}

func (policy *writerAdmissionPolicy) AcceptOutboundTerminal() error { return policy.terminalErr }
func (policy *writerAdmissionPolicy) OutboundDirection() Direction  { return policy.direction }

func TestSessionWriterCancellationAdmissionIsAtomicAndFailClosed(t *testing.T) {
	operationID := testOperationID(240)
	cancelMessage := mustMessage(t, MessageCancel, &operationID, map[uint64]any{0: uint64(1)})
	replay := testReplayPermit(cancelMessage, DirectionReceiverToSender)

	t.Run("deliver", func(t *testing.T) {
		policy := &writerAdmissionPolicy{
			direction: DirectionReceiverToSender,
			admission: OutboundAdmission{Disposition: OperationDeliver, Replay: replay},
		}
		writer, err := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := writer.TryControl(cancelMessage)
		if err != nil {
			t.Fatal(err)
		}
		completion := runWriterToCompletion(t, writer, receipt)
		if completion.Outcome != SendOutcomeDelivered || !completion.Admitted || completion.Replay.IsZero() ||
			policy.admitCalls != 1 {
			t.Fatalf("cancellation completion=%+v admission calls=%d", completion, policy.admitCalls)
		}
	})

	t.Run("drop", func(t *testing.T) {
		policy := &writerAdmissionPolicy{
			direction: DirectionReceiverToSender,
			admission: OutboundAdmission{Disposition: OperationDrop},
		}
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
		receipt, err := writer.TryControl(cancelMessage)
		if err != nil {
			t.Fatal(err)
		}
		completion := receipt.Await(context.Background())
		if completion.Outcome != SendOutcomeDropped || !completion.Settled || completion.Err != nil {
			t.Fatalf("dropped cancellation completion=%+v", completion)
		}
	})

	t.Run("policy failure", func(t *testing.T) {
		cause := errors.New("cancellation admission failed")
		policy := &writerAdmissionPolicy{direction: DirectionReceiverToSender, admitErr: cause}
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
		if _, err := writer.TryControl(cancelMessage); !errors.Is(err, cause) {
			t.Fatalf("cancellation policy error=%v", err)
		}
	})

	t.Run("missing replay authority", func(t *testing.T) {
		policy := &writerAdmissionPolicy{
			direction: DirectionReceiverToSender,
			admission: OutboundAdmission{Disposition: OperationDeliver},
		}
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
		if _, err := writer.TryControl(cancelMessage); !errors.Is(err, ErrOutboundReplayPermit) {
			t.Fatalf("permit-less cancellation error=%v", err)
		}
	})

	t.Run("writer state and queue bounds", func(t *testing.T) {
		newWriter := func() *SessionWriter {
			policy := &writerAdmissionPolicy{
				direction: DirectionReceiverToSender,
				admission: OutboundAdmission{Disposition: OperationDeliver, Replay: replay},
			}
			writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
			return writer
		}

		terminal := newWriter()
		terminal.state = writerTerminalAccepted
		if _, err := terminal.TryControl(cancelMessage); !errors.Is(err, ErrWriterTerminal) {
			t.Fatalf("terminal cancellation error=%v", err)
		}

		stopped := newWriter()
		stopped.state = writerStopped
		if _, err := stopped.TryControl(cancelMessage); !errors.Is(err, ErrWriterStopped) {
			t.Fatalf("stopped cancellation error=%v", err)
		}

		full := newWriter()
		full.controlUsage.frames = ControlQueueFrameLimit
		if _, err := full.TryControl(cancelMessage); !errors.Is(err, ErrControlQueueFull) {
			t.Fatalf("full cancellation queue error=%v", err)
		}
	})
}

func TestSessionWriterContinuationAdmissionCommitsOnlyQueuedDelivery(t *testing.T) {
	operationID := testOperationID(241)
	candidate := testContinuationCandidate(t, operationID, 21, "candidate")

	t.Run("deliver", func(t *testing.T) {
		policy := &writerAdmissionPolicy{
			direction: DirectionReceiverToSender,
			admission: OutboundAdmission{Disposition: OperationDeliver},
		}
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
		receipt, err := writer.TryControl(candidate)
		if err != nil {
			t.Fatal(err)
		}
		completion := runWriterToCompletion(t, writer, receipt)
		if completion.Outcome != SendOutcomeDelivered || !completion.Admitted || policy.admitCalls != 1 {
			t.Fatalf("continuation completion=%+v admission calls=%d", completion, policy.admitCalls)
		}
	})

	t.Run("drop and policy failure", func(t *testing.T) {
		drop := &writerAdmissionPolicy{
			direction: DirectionReceiverToSender,
			admission: OutboundAdmission{Disposition: OperationDrop},
		}
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, drop)
		receipt, err := writer.TryControl(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if completion := receipt.Await(context.Background()); completion.Outcome != SendOutcomeDropped ||
			!completion.Settled {
			t.Fatalf("dropped continuation completion=%+v", completion)
		}

		cause := errors.New("continuation admission failed")
		failed := &writerAdmissionPolicy{direction: DirectionReceiverToSender, admitErr: cause}
		writer, _ = NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, failed)
		if _, err := writer.TryControl(candidate); !errors.Is(err, cause) {
			t.Fatalf("continuation policy error=%v", err)
		}
	})

	t.Run("sender replay and replay requirement", func(t *testing.T) {
		prepared := preparedWriterControl(t, candidate)
		permit := testReplayPermit(candidate, DirectionSenderToReceiver)
		missing := &writerAdmissionPolicy{
			direction: DirectionSenderToReceiver,
			admission: OutboundAdmission{Disposition: OperationDeliver},
		}
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, missing)
		if _, err := writer.TrySenderControl(prepared); !errors.Is(err, ErrOutboundReplayPermit) {
			t.Fatalf("permit-less sender continuation error=%v", err)
		}

		replayPolicy := &writerAdmissionPolicy{
			direction:       DirectionSenderToReceiver,
			replayAdmission: OutboundAdmission{Disposition: OperationDeliver, Replay: permit},
		}
		writer, _ = NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, replayPolicy)
		receipt, err := writer.TrySenderControlReplay(prepared, permit)
		if err != nil {
			t.Fatal(err)
		}
		completion := runWriterToCompletion(t, writer, receipt)
		if completion.Outcome != SendOutcomeDelivered || replayPolicy.replayCalls != 1 {
			t.Fatalf("replayed continuation completion=%+v replay calls=%d", completion, replayPolicy.replayCalls)
		}
	})

	t.Run("writer state and queue bounds", func(t *testing.T) {
		newWriter := func() *SessionWriter {
			policy := &writerAdmissionPolicy{
				direction: DirectionReceiverToSender,
				admission: OutboundAdmission{Disposition: OperationDeliver},
			}
			writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
			return writer
		}
		terminal := newWriter()
		terminal.state = writerTerminalAccepted
		if _, err := terminal.TryControl(candidate); !errors.Is(err, ErrWriterTerminal) {
			t.Fatalf("terminal continuation error=%v", err)
		}
		stopped := newWriter()
		stopped.state = writerStopped
		if _, err := stopped.TryControl(candidate); !errors.Is(err, ErrWriterStopped) {
			t.Fatalf("stopped continuation error=%v", err)
		}
		full := newWriter()
		full.controlUsage.frames = ControlQueueFrameLimit
		if _, err := full.TryControl(candidate); !errors.Is(err, ErrControlQueueFull) {
			t.Fatalf("full continuation queue error=%v", err)
		}
	})
}

func TestSessionWriterReplayWrappersAndPreparationContracts(t *testing.T) {
	var nilItem *queuedMessage
	nilItem.releasePin()
	nilItem.settleContinuation(true)
	nilItem.settleContinuation(false)

	operationID := testOperationID(242)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	response := mustMessage(t, MessageOperationComplete, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := (&SessionWriter{}).TryControlObservingAuthenticatedViolations(request, nil); !errors.Is(err, ErrAuthenticatedOperationObserver) {
		t.Fatalf("nil authenticated-violation observer error=%v", err)
	}
	if _, err := (&SessionWriter{}).TryControlObservingAuthenticatedViolations(response, func(AuthenticatedOperationViolation) {}); !errors.Is(err, ErrMessageClass) {
		t.Fatalf("response observer error=%v", err)
	}

	receiverReplay := testReplayPermit(request, DirectionReceiverToSender)
	receiverPolicy := &writerAdmissionPolicy{
		direction:       DirectionReceiverToSender,
		replayAdmission: OutboundAdmission{Disposition: OperationDeliver, Replay: receiverReplay},
	}
	receiverWriter, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, receiverPolicy)
	receipt, err := receiverWriter.TryControlReplay(request, receiverReplay)
	if err != nil {
		t.Fatal(err)
	}
	if completion := runWriterToCompletion(t, receiverWriter, receipt); completion.Outcome != SendOutcomeDelivered ||
		receiverPolicy.replayCalls != 1 {
		t.Fatalf("receiver replay completion=%+v replay calls=%d", completion, receiverPolicy.replayCalls)
	}

	fragment := mustFragmentMessage(t, operationID, 7)
	senderReplay := testReplayPermit(fragment, DirectionSenderToReceiver)
	for name, submit := range map[string]func(*SessionWriter) (SendReceipt, error){
		"authorized data": func(writer *SessionWriter) (SendReceipt, error) {
			return writer.TryAuthorizedData(fragment, OutboundOperationPermit{})
		},
		"data replay": func(writer *SessionWriter) (SendReceipt, error) {
			return writer.TryDataReplay(fragment, senderReplay)
		},
	} {
		t.Run(name, func(t *testing.T) {
			policy := &writerAdmissionPolicy{
				direction:       DirectionSenderToReceiver,
				admission:       OutboundAdmission{Disposition: OperationDeliver, Replay: senderReplay},
				replayAdmission: OutboundAdmission{Disposition: OperationDeliver, Replay: senderReplay},
			}
			writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
			receipt, err := submit(writer)
			if err != nil {
				t.Fatal(err)
			}
			if completion := runWriterToCompletion(t, writer, receipt); completion.Outcome != SendOutcomeDelivered {
				t.Fatalf("data completion=%+v", completion)
			}
		})
	}

	receiverWriter, _ = NewSessionWriter(
		newRuntimeChannel(0),
		&passthroughSealer{},
		&writerAdmissionPolicy{direction: DirectionReceiverToSender},
	)
	if _, err := receiverWriter.TryControl(Message{}); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid control preparation error=%v", err)
	}
	invalidData := Message{kind: MessageBlockFragment}
	if _, err := receiverWriter.TryData(invalidData); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid data preparation error=%v", err)
	}
	if _, err := receiverWriter.TrySenderControl(PreparedSenderControl{}); !errors.Is(err, ErrSequenceBoundControl) {
		t.Fatalf("invalid sender control error=%v", err)
	}

	oversized := Message{
		kind:      MessageListChildren,
		plaintext: make([]byte, MaxEnvelopePlaintextBytes+1),
	}
	if _, err := prepareQueuedMessage(oversized, classControl); !errors.Is(err, ErrEnvelopeTooLarge) {
		t.Fatalf("oversized queued message error=%v", err)
	}
	if _, err := prepareSequencedMessage(
		MaxEnvelopePlaintextBytes+1,
		classControl,
		func(uint64) (Message, error) { return request, nil },
	); !errors.Is(err, ErrEnvelopeTooLarge) {
		t.Fatalf("oversized sequenced message error=%v", err)
	}
	oversizedControl := PreparedSenderControl{
		plaintextBytes: MaxEnvelopePlaintextBytes + 1,
		kind:           MessageOperationComplete,
		intent:         response,
		build:          func(uint64) (Message, error) { return response, nil },
	}
	senderWriter, _ := NewSessionWriter(
		newRuntimeChannel(0),
		&passthroughSealer{},
		&writerAdmissionPolicy{direction: DirectionSenderToReceiver},
	)
	if _, err := senderWriter.TrySenderControl(oversizedControl); !errors.Is(err, ErrEnvelopeTooLarge) {
		t.Fatalf("oversized sender control error=%v", err)
	}

	stoppedContext, stop := context.WithCancel(context.Background())
	stop()
	if err := senderWriter.Run(stoppedContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop writer error=%v", err)
	}
	if _, err := senderWriter.TryData(fragment); !errors.Is(err, ErrWriterStopped) {
		t.Fatalf("post-stop data error=%v", err)
	}
	if _, err := senderWriter.TrySenderControl(mustPreparedControl(t, MessageSessionTerminal, nil)); !errors.Is(err, ErrWriterStopped) {
		t.Fatalf("post-stop terminal error=%v", err)
	}
}

func preparedWriterControl(t *testing.T, message Message) PreparedSenderControl {
	t.Helper()
	plaintext := mustPlaintext(t, message)
	return PreparedSenderControl{
		plaintextBytes: len(plaintext),
		kind:           message.Kind(),
		intent:         message,
		build:          func(uint64) (Message, error) { return message, nil },
	}
}

func runWriterToCompletion(t *testing.T, writer *SessionWriter, receipt SendReceipt) SendCompletion {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Run(ctx) }()
	completion := receipt.Await(context.Background())
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("writer stop error=%v", err)
	}
	return completion
}
