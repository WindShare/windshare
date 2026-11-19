package protocolsession

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"

	"github.com/windshare/windshare/core/senderauth"
)

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

	prepared := mustWriterControl(t, MessageOperationError, &operationID)
	controlReceipt, err := writer.TryControl(prepared)
	if err != nil {
		t.Fatal(err)
	}
	close(channel.releaseFirst)
	if outcome, err := controlReceipt.Wait(context.Background()); err != nil || outcome != SendOutcomeTransportConfirmed {
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
		if _, err := writer.TryControl(mustWriterControl(t, MessageOperationError, &operationID)); err != nil {
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
	if outcome, waitErr := dataReceipt.Wait(context.Background()); outcome != SendOutcomeTransportConfirmed || waitErr != nil {
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
	operations, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTracked: 4}, nil)
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
	terminal := mustWriterControl(t, MessageSessionTerminal, nil)
	terminalReceipt, err := writer.TryTerminal(terminal)
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
	if _, err := writer.TryTerminal(terminal); !errors.Is(err, ErrWriterTerminal) {
		t.Fatalf("duplicate terminal = %v", err)
	}
	if err := writer.Run(context.Background()); err != nil {
		t.Fatalf("terminal writer run: %v", err)
	}
	if outcome, err := terminalReceipt.Wait(context.Background()); err != nil || outcome != SendOutcomeTransportConfirmed {
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
	controlOperationID := testOperationID(80)
	controlMessage := mustMessage(t, MessageListChildren, &controlOperationID, map[uint64]any{0: uint64(1)})
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
	if _, err := dataWriter.TryControl(data); !errors.Is(err, ErrMessageClass) {
		t.Fatalf("data through control queue = %v", err)
	}
	if _, err := controlWriter.TryData(controlMessage); !errors.Is(err, ErrMessageClass) {
		t.Fatalf("control through data queue = %v", err)
	}
	if outcome, err := (SendReceipt{}).Wait(context.Background()); !errors.Is(err, ErrWriterStopped) || outcome != SendOutcomeUninitialized {
		t.Fatalf("zero receipt outcome=%d err=%v", outcome, err)
	}
	if (SendReceipt{}).Done() != nil {
		t.Fatal("zero receipt has a completion channel")
	}
}

func TestSessionWriterDropsWithoutConsumingSequence(t *testing.T) {
	operationID := testOperationID(124)
	message := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	dropPolicy := &dispositionPolicy{direction: DirectionReceiverToSender, disposition: OperationDrop}
	sealer := &passthroughSealer{next: 9}
	writer, _ := NewSessionWriter(newRuntimeChannel(0), sealer, dropPolicy)
	receipt, err := writer.TryControl(message)
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
			receipt, err := writer.TryControl(message)
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
	if _, err := terminalWriter.TryTerminal(mustWriterControl(t, MessageSessionTerminal, nil)); err == nil {
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

func mustWriterControl(t *testing.T, kind MessageKind, operationID *OperationID) Message {
	t.Helper()
	if kind == MessageSessionTerminal {
		return mustSessionTerminalMessage(t)
	}
	return mustMessage(t, kind, operationID, map[uint64]any{0: uint64(1), 1: uint64(2)})
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

func checkedSender(t testing.TB, publicKey []byte) *senderauth.Verifier {
	t.Helper()
	sender, err := senderauth.NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}
