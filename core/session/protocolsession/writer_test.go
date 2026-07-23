package protocolsession

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

func TestSessionWriterSignsTheExactEmittedSequence(t *testing.T) {
	vector, key, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	operationID, err := OperationIDFromBytes(decodeB64(t, vector.OperationIDB64))
	if err != nil {
		t.Fatal(err)
	}
	operations, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	router, _ := NewRoleRouter(RoleSender, operations)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	operationAdmission, err := operations.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil {
		t.Fatal(err)
	}

	sealer, err := NewEnvelopeSealer(key, envelopeBinding, bytes.NewReader(sequentialBytes(0x40, EnvelopeNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	const emittedSequence = uint64(41)
	sealer.next = emittedSequence
	channel := newRuntimeChannel(0)
	writer, err := NewSessionWriter(channel, sealer, router)
	if err != nil {
		t.Fatal(err)
	}

	signingKey := vectorSenderSigningKey()
	publicKey := append(ed25519.PublicKey(nil), signingKey.Public().(ed25519.PublicKey)...)
	unsigned, err := EncodeOperationFailure(OperationFailure{
		Scope: OperationScopePeer, Code: PeerOperationCodeNegotiation, Message: "Peer negotiation failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: envelopeBinding.LaneID, LaneEpoch: envelopeBinding.LaneEpoch, Direction: DirectionSenderToReceiver,
	}
	prepared, err := PrepareSenderControl(signingKey, base, MessageOperationError, &operationID, unsigned)
	if err != nil {
		t.Fatal(err)
	}
	// Prepared controls own their signing material and body until the writer
	// invokes the callback.
	signingKey[0] ^= 1
	unsigned[0] ^= 1
	receipt, err := writer.TryAuthorizedSenderControl(prepared, operationAdmission.Outbound)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- writer.Run(ctx) }()
	if outcome, err := receipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
		t.Fatalf("send signed control: outcome=%d err=%v", outcome, err)
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("writer stop = %v", err)
	}

	channel.mu.Lock()
	if len(channel.sent) != 1 {
		channel.mu.Unlock()
		t.Fatalf("sent frames = %d", len(channel.sent))
	}
	frame := append([]byte(nil), channel.sent[0]...)
	channel.mu.Unlock()
	opener := mustOpener(t, key, envelopeBinding)
	opener.next = emittedSequence
	opened, err := opener.Open(frame)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Sequence != emittedSequence {
		t.Fatalf("emitted sequence = %d, want %d", opened.Sequence, emittedSequence)
	}
	message, err := DecodeMessage(opened.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := NewSenderControlAuthenticator(publicKey, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.AuthenticateInbound(opened.Sequence, message); err != nil {
		t.Fatalf("signature did not bind emitted sequence: %v", err)
	}
	if _, err := authenticator.AuthenticateInbound(opened.Sequence+1, message); !errors.Is(err, ErrControlSignature) {
		t.Fatalf("signature accepted a different sequence: %v", err)
	}
}

func TestSessionWriterControlPriorityAndFairDataBurst(t *testing.T) {
	policy := &permissivePolicy{direction: DirectionSenderToReceiver}
	sealer := &passthroughSealer{}
	channel := newGatedWriterChannel()
	writer, err := NewSessionWriter(channel, sealer, policy)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(60)
	for marker := range byte(16) {
		if _, err := writer.TryData(mustFragmentMessage(t, operationID, marker)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- writer.Run(ctx) }()
	<-channel.firstStarted

	prepared := mustPreparedControl(t, MessageOperationError, &operationID)
	controlReceipt, err := writer.TrySenderControl(prepared)
	if err != nil {
		t.Fatal(err)
	}
	close(channel.releaseFirst)
	if outcome, err := controlReceipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
		t.Fatalf("control outcome=%d err=%v", outcome, err)
	}
	cancel()
	<-runDone

	frames := channel.frames()
	controlIndex := -1
	dataBeforeControl := 0
	for index, frame := range frames {
		message, err := DecodeMessage(frame)
		if err != nil {
			t.Fatalf("decode sent frame %d: %v", index, err)
		}
		if message.IsData() {
			if controlIndex < 0 {
				dataBeforeControl++
			}
			continue
		}
		controlIndex = index
		break
	}
	if controlIndex < 0 || dataBeforeControl > MaximumDataBurst {
		t.Fatalf("control index=%d data before control=%d, limit=%d", controlIndex, dataBeforeControl, MaximumDataBurst)
	}
}

func TestSessionWriterSustainedControlStillAdvancesData(t *testing.T) {
	policy := &permissivePolicy{direction: DirectionSenderToReceiver}
	writer, err := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(61)
	for range MaximumControlBurst * 2 {
		if _, err := writer.TrySenderControl(mustPreparedControl(t, MessageOperationError, &operationID)); err != nil {
			t.Fatal(err)
		}
	}
	dataReceipt, err := writer.TryData(mustFragmentMessage(t, operationID, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Run(ctx) }()
	if outcome, waitErr := dataReceipt.Wait(context.Background()); outcome != SendOutcomeDelivered || waitErr != nil {
		t.Fatalf("data receipt = %d, %v", outcome, waitErr)
	}
	cancel()
	<-done

	channel := writer.channel.(*runtimeChannel)
	channel.mu.Lock()
	frames := append([]framechannel.Frame(nil), channel.sent...)
	channel.mu.Unlock()
	dataIndex := -1
	for index, frame := range frames {
		message, decodeErr := DecodeMessage(frame)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if message.IsData() {
			dataIndex = index
			break
		}
	}
	if dataIndex < 0 || dataIndex > MaximumControlBurst {
		t.Fatalf("data index = %d, control burst limit = %d", dataIndex, MaximumControlBurst)
	}
}

func TestSessionWriterTerminalAdmissionIsImmediateAndOutOfBand(t *testing.T) {
	operations, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	router, _ := NewRoleRouter(RoleSender, operations)
	operationID := testOperationID(70)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	_, _ = router.RouteInbound(context.Background(), request)
	channel := newRuntimeChannel(0)
	writer, _ := NewSessionWriter(channel, &passthroughSealer{}, router)
	if !writer.Accepting() {
		t.Fatal("new writer did not expose accepting authority")
	}
	dataReceipt, err := writer.TryData(mustFragmentMessage(t, operationID, 1))
	if err != nil {
		t.Fatal(err)
	}
	terminal := mustPreparedControl(t, MessageSessionTerminal, nil)
	terminalReceipt, err := writer.TrySenderControl(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if !operations.Terminated() {
		t.Fatal("terminal admission left operation registry open")
	}
	if writer.Accepting() {
		t.Fatal("terminal-accepted writer remained selectable")
	}
	if outcome, err := dataReceipt.Wait(context.Background()); !errors.Is(err, ErrWriterTerminal) || outcome != SendOutcomeDropped {
		t.Fatalf("queued data receipt outcome=%d err=%v", outcome, err)
	}
	if _, err := writer.TryData(mustFragmentMessage(t, operationID, 2)); !errors.Is(err, ErrWriterTerminal) {
		t.Fatalf("post-terminal data = %v", err)
	}
	if _, err := writer.TrySenderControl(terminal); !errors.Is(err, ErrWriterTerminal) {
		t.Fatalf("duplicate terminal = %v", err)
	}
	if err := writer.Run(context.Background()); err != nil {
		t.Fatalf("terminal writer run: %v", err)
	}
	if outcome, err := terminalReceipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
		t.Fatalf("terminal receipt outcome=%d err=%v", outcome, err)
	}
	channel.mu.Lock()
	sent, terminals := len(channel.sent), len(channel.terminal)
	channel.mu.Unlock()
	if sent != 0 || terminals != 1 {
		t.Fatalf("transport calls ordinary=%d terminal=%d", sent, terminals)
	}
	if writer.Err() != nil {
		t.Fatalf("writer terminal error = %v", writer.Err())
	}
	if writer.Accepting() {
		t.Fatal("completed writer regained accepting authority")
	}
	<-writer.Done()
}

func TestSessionWriterBoundedQueuesClassesAndStopReceipts(t *testing.T) {
	controlMessage := mustMessage(t, MessageListChildren, new(testOperationID(80)), map[uint64]any{0: uint64(1)})
	controlWriter, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, &permissivePolicy{direction: DirectionReceiverToSender})
	receipts := make([]SendReceipt, 0, ControlQueueFrameLimit)
	for range ControlQueueFrameLimit {
		receipt, err := controlWriter.TryControl(controlMessage)
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	if _, err := controlWriter.TryControl(controlMessage); !errors.Is(err, ErrControlQueueFull) {
		t.Fatalf("control frame bound: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controlWriter.Run(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled writer = %v", err)
	}
	if outcome, err := receipts[0].Wait(context.Background()); !errors.Is(err, ErrWriterStopped) || outcome != SendOutcomeDropped {
		t.Fatalf("stopped queue receipt outcome=%d err=%v", outcome, err)
	}
	if controlWriter.Err() == nil {
		t.Fatal("stopped writer lost its run error")
	}
	if err := controlWriter.Run(context.Background()); !errors.Is(err, ErrWriterReused) {
		t.Fatalf("writer reuse = %v", err)
	}

	operationID := testOperationID(81)
	data := mustFragmentMessage(t, operationID, 1)
	dataWriter, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, &permissivePolicy{direction: DirectionSenderToReceiver})
	for range DataQueueFrameLimit {
		if _, err := dataWriter.TryData(data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dataWriter.TryData(data); !errors.Is(err, ErrDataQueueFull) {
		t.Fatalf("data frame bound: %v", err)
	}
	unsignedSenderControl := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
	if _, err := dataWriter.TryControl(unsignedSenderControl); !errors.Is(err, ErrSequenceBoundControl) {
		t.Fatalf("unsigned sender control = %v", err)
	}
	if _, err := dataWriter.tryControlWithSequence(1, func(uint64) (Message, error) { return controlMessage, nil }); !errors.Is(err, ErrSequenceBoundControl) {
		t.Fatalf("raw sender builder = %v", err)
	}
	if _, err := dataWriter.TryControl(data); !errors.Is(err, ErrMessageClass) {
		t.Fatalf("data through control queue = %v", err)
	}
	if _, err := controlWriter.TryData(controlMessage); !errors.Is(err, ErrMessageClass) {
		t.Fatalf("control through data queue = %v", err)
	}
	if outcome, err := (SendReceipt{}).Wait(context.Background()); !errors.Is(err, ErrWriterStopped) || outcome != SendOutcomeUnknown {
		t.Fatalf("zero receipt outcome=%d err=%v", outcome, err)
	}
	if (SendReceipt{}).Done() != nil {
		t.Fatal("zero receipt has a completion channel")
	}
}

func TestSessionWriterPropagatesBuilderSealerAndTransportFailures(t *testing.T) {
	operationID := testOperationID(90)
	policy := &permissivePolicy{direction: DirectionReceiverToSender}
	tests := []struct {
		name    string
		sealer  OutboundEnvelopeSealer
		channel FrameChannel
		build   sequencedMessageBuilder
		size    int
		want    error
	}{
		{
			name: "builder", sealer: &passthroughSealer{}, channel: newRuntimeChannel(0),
			build: func(uint64) (Message, error) { return Message{}, errors.New("build failed") }, size: 1, want: ErrSequencedBuild,
		},
		{
			name: "sealer sequence", sealer: &wrongSequenceSealer{}, channel: newRuntimeChannel(0),
			build: func(uint64) (Message, error) {
				return mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)}), nil
			},
			size: len(mustPlaintext(t, mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)}))), want: ErrSealerSequence,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer, _ := NewSessionWriter(test.channel, test.sealer, policy)
			receipt, err := writer.tryControlWithSequence(test.size, test.build)
			if err != nil {
				t.Fatal(err)
			}
			runErr := writer.Run(context.Background())
			if !errors.Is(runErr, test.want) {
				t.Fatalf("run error = %v, want %v", runErr, test.want)
			}
			if outcome, err := receipt.Wait(context.Background()); !errors.Is(err, test.want) || outcome != SendOutcomeDropped {
				t.Fatalf("receipt outcome=%d error=%v, want %v", outcome, err, test.want)
			}
		})
	}
	if _, err := NewSessionWriter(nil, &passthroughSealer{}, policy); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil writer dependency = %v", err)
	}
	if _, err := PrepareSenderControl(nil, ControlBinding{}, MessageOperationError, &operationID, []byte{0xa0}); !errors.Is(err, ErrControlSigningKey) {
		t.Fatalf("invalid prepared key = %v", err)
	}
}

func TestSessionWriterRejectsSequencedMismatchAndPolicyDropsWithoutConsumingSequence(t *testing.T) {
	operationID := testOperationID(124)
	message := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	plaintext := mustPlaintext(t, message)
	for name, candidate := range map[string]struct {
		reserved int
		build    sequencedMessageBuilder
		want     error
	}{
		"nil builder":      {reserved: len(plaintext)},
		"zero reservation": {build: func(uint64) (Message, error) { return message, nil }},
		"size mismatch": {
			reserved: len(plaintext) + 1,
			build:    func(uint64) (Message, error) { return message, nil },
			want:     ErrSequencedSize,
		},
		"wrong class": {
			reserved: len(mustPlaintext(t, mustFragmentMessage(t, operationID, 1))),
			build:    func(uint64) (Message, error) { return mustFragmentMessage(t, operationID, 1), nil },
			want:     ErrMessageClass,
		},
	} {
		t.Run(name, func(t *testing.T) {
			writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, &permissivePolicy{direction: DirectionReceiverToSender})
			receipt, err := writer.tryControlWithSequence(candidate.reserved, candidate.build)
			if candidate.want == nil {
				if !errors.Is(err, ErrSequencedBuild) {
					t.Fatalf("admission error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Run(context.Background()); !errors.Is(err, candidate.want) {
				t.Fatalf("run error = %v, want %v", err, candidate.want)
			}
			if outcome, err := receipt.Wait(context.Background()); !errors.Is(err, candidate.want) || outcome != SendOutcomeDropped {
				t.Fatalf("receipt outcome=%d err=%v", outcome, err)
			}
		})
	}

	dropPolicy := &dispositionPolicy{direction: DirectionReceiverToSender, disposition: OperationDrop}
	sealer := &passthroughSealer{next: 9}
	writer, _ := NewSessionWriter(newRuntimeChannel(0), sealer, dropPolicy)
	receipt, err := writer.tryControlWithSequence(len(plaintext), func(uint64) (Message, error) { return message, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Run(ctx) }()
	if outcome, err := receipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDropped {
		t.Fatalf("policy drop outcome=%d err=%v", outcome, err)
	}
	if sealer.next != 9 {
		t.Fatalf("dropped control consumed sequence %d", sealer.next)
	}
	cancel()
	<-done
}

func TestSessionWriterPropagatesSequenceTransportAndPolicyErrors(t *testing.T) {
	operationID := testOperationID(125)
	message := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	plaintext := mustPlaintext(t, message)
	tests := []struct {
		name    string
		sealer  OutboundEnvelopeSealer
		channel FrameChannel
		policy  OutboundMessagePolicy
		want    error
	}{
		{"next sequence", &failingSealer{nextErr: errors.New("next")}, newRuntimeChannel(0), &permissivePolicy{direction: DirectionReceiverToSender}, errors.New("next")},
		{"seal", &failingSealer{sealErr: errors.New("seal")}, newRuntimeChannel(0), &permissivePolicy{direction: DirectionReceiverToSender}, errors.New("seal")},
		{"transport", &passthroughSealer{}, &failingChannel{sendErr: errors.New("transport")}, &permissivePolicy{direction: DirectionReceiverToSender}, errors.New("transport")},
		{"policy", &passthroughSealer{}, newRuntimeChannel(0), &dispositionPolicy{direction: DirectionReceiverToSender, err: errors.New("policy")}, errors.New("policy")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer, _ := NewSessionWriter(test.channel, test.sealer, test.policy)
			receipt, err := writer.tryControlWithSequence(len(plaintext), func(uint64) (Message, error) { return message, nil })
			if err != nil {
				t.Fatal(err)
			}
			runErr := writer.Run(context.Background())
			if runErr == nil || runErr.Error() == "" {
				t.Fatalf("run error = %v", runErr)
			}
			outcome, err := receipt.Wait(context.Background())
			if err == nil {
				t.Fatal("receipt lost writer failure")
			}
			wantOutcome := SendOutcomeDropped
			if test.name == "transport" {
				wantOutcome = SendOutcomeUnknown
			}
			if outcome != wantOutcome {
				t.Fatalf("receipt outcome=%d want=%d err=%v", outcome, wantOutcome, err)
			}
		})
	}

	terminalPolicy := &dispositionPolicy{direction: DirectionSenderToReceiver, terminalErr: errors.New("terminal policy")}
	terminalWriter, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, terminalPolicy)
	if _, err := terminalWriter.TrySenderControl(mustPreparedControl(t, MessageSessionTerminal, nil)); err == nil {
		t.Fatal("terminal policy error was ignored")
	}

	writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, &permissivePolicy{direction: DirectionReceiverToSender})
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	receipt, _ := writer.TryControl(message)
	if outcome, err := receipt.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) || outcome != SendOutcomeDropped {
		t.Fatalf("receipt deadline outcome=%d err=%v", outcome, err)
	}
}

type permissivePolicy struct {
	direction  Direction
	terminated bool
}

type dispositionPolicy struct {
	direction   Direction
	disposition OperationDisposition
	err         error
	terminalErr error
}

func (p *dispositionPolicy) AdmitOutbound(
	message Message,
	_ OutboundOperationPermit,
) (OutboundAdmission, error) {
	admission := OutboundAdmission{Disposition: p.disposition}
	if p.direction == DirectionSenderToReceiver && p.disposition == OperationDeliver {
		admission.Replay = testReplayPermit(message, p.direction)
	}
	return admission, p.err
}

func (p *dispositionPolicy) AcceptOutboundReplay(Message, OutboundReplayPermit) (OutboundAdmission, error) {
	return OutboundAdmission{Disposition: p.disposition}, p.err
}

func (p *dispositionPolicy) AcceptOutboundTerminal() error { return p.terminalErr }
func (p *dispositionPolicy) OutboundDirection() Direction  { return p.direction }

func (p *permissivePolicy) AdmitOutbound(
	message Message,
	_ OutboundOperationPermit,
) (OutboundAdmission, error) {
	if p.terminated {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	admission := OutboundAdmission{Disposition: OperationDeliver}
	if p.direction == DirectionSenderToReceiver {
		admission.Replay = testReplayPermit(message, p.direction)
	}
	return admission, nil
}

func (p *permissivePolicy) AcceptOutboundReplay(Message, OutboundReplayPermit) (OutboundAdmission, error) {
	if p.terminated {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	return OutboundAdmission{Disposition: OperationDeliver}, nil
}

func (p *permissivePolicy) AcceptOutboundTerminal() error {
	if p.direction != DirectionSenderToReceiver {
		return ErrInvalidDirection
	}
	if p.terminated {
		return ErrSessionTerminated
	}
	p.terminated = true
	return nil
}

func (p *permissivePolicy) OutboundDirection() Direction { return p.direction }

func testReplayPermit(message Message, direction Direction) OutboundReplayPermit {
	operationID, _ := message.OperationID()
	return OutboundReplayPermit{
		table: &OperationTable{}, authority: &operationAuthority{}, direction: direction,
		kind: message.kind, operationID: operationID, fingerprint: message.operationFingerprint(direction),
	}
}

type passthroughSealer struct{ next uint64 }

func (s *passthroughSealer) NextSequence() (uint64, error) { return s.next, nil }

func (s *passthroughSealer) Seal(plaintext []byte) (SealedEnvelope, error) {
	sequence := s.next
	s.next++
	return SealedEnvelope{Sequence: sequence, Frame: append([]byte(nil), plaintext...)}, nil
}

type wrongSequenceSealer struct{}

func (*wrongSequenceSealer) NextSequence() (uint64, error) { return 4, nil }
func (*wrongSequenceSealer) Seal(plaintext []byte) (SealedEnvelope, error) {
	return SealedEnvelope{Sequence: 5, Frame: append([]byte(nil), plaintext...)}, nil
}

type failingSealer struct {
	nextErr error
	sealErr error
}

func (s *failingSealer) NextSequence() (uint64, error) { return 0, s.nextErr }
func (s *failingSealer) Seal([]byte) (SealedEnvelope, error) {
	return SealedEnvelope{}, s.sealErr
}

type failingChannel struct{ sendErr error }

func (c *failingChannel) Send(context.Context, framechannel.Frame) error { return c.sendErr }
func (c *failingChannel) SendTerminal(context.Context, framechannel.Frame) error {
	return c.sendErr
}
func (*failingChannel) Recv() <-chan framechannel.Frame  { return nil }
func (*failingChannel) State() framechannel.ChannelState { return framechannel.Open }
func (*failingChannel) Close() error                     { return nil }

type gatedWriterChannel struct {
	mu           sync.Mutex
	sent         []framechannel.Frame
	firstStarted chan struct{}
	releaseFirst chan struct{}
	once         sync.Once
}

func newGatedWriterChannel() *gatedWriterChannel {
	return &gatedWriterChannel{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (c *gatedWriterChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	blocked := false
	c.once.Do(func() {
		blocked = true
		close(c.firstStarted)
	})
	if blocked {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.releaseFirst:
		}
	}
	c.mu.Lock()
	c.sent = append(c.sent, append(framechannel.Frame(nil), frame...))
	c.mu.Unlock()
	return nil
}

func (c *gatedWriterChannel) SendTerminal(context.Context, framechannel.Frame) error { return nil }
func (c *gatedWriterChannel) Recv() <-chan framechannel.Frame                        { return nil }
func (c *gatedWriterChannel) State() framechannel.ChannelState                       { return framechannel.Open }
func (c *gatedWriterChannel) Close() error                                           { return nil }

func (c *gatedWriterChannel) frames() []framechannel.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]framechannel.Frame, len(c.sent))
	for index := range c.sent {
		result[index] = append(framechannel.Frame(nil), c.sent[index]...)
	}
	return result
}

func mustPreparedControl(t *testing.T, kind MessageKind, operationID *OperationID) PreparedSenderControl {
	t.Helper()
	_, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	base := ControlBinding{
		ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
		LaneID: envelopeBinding.LaneID, LaneEpoch: envelopeBinding.LaneEpoch, Direction: DirectionSenderToReceiver,
	}
	prepared, err := PrepareSenderControl(
		vectorSenderSigningKey(), base, kind, operationID,
		mustControlBody(t, map[uint64]any{0: uint64(1), 1: uint64(2)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func mustPlaintext(t *testing.T, message Message) []byte {
	t.Helper()
	plaintext, err := EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func (p *passthroughSealer) String() string { return fmt.Sprintf("sequence=%d", p.next) }
