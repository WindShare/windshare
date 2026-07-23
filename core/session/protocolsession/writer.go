package protocolsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

const (
	ControlQueueFrameLimit = 256
	ControlQueueByteLimit  = 4 * 1024 * 1024
	DataQueueFrameLimit    = 1024
	DataQueueByteLimit     = 64 * 1024 * 1024
	MaximumDataBurst       = 8
	MaximumControlBurst    = 8
)

var (
	ErrControlQueueFull     = errors.New("protocolsession: control queue capacity exceeded")
	ErrDataQueueFull        = errors.New("protocolsession: data queue capacity exceeded")
	ErrWriterTerminal       = errors.New("protocolsession: writer accepted a terminal message")
	ErrWriterStopped        = errors.New("protocolsession: writer is stopped")
	ErrWriterReused         = errors.New("protocolsession: writer Run may only be called once")
	ErrMessageClass         = errors.New("protocolsession: message submitted to the wrong writer queue")
	ErrSequenceBoundControl = errors.New("protocolsession: sender control must be built with its envelope sequence")
	ErrSequencedBuild       = errors.New("protocolsession: sequenced message builder failed")
	ErrSequencedSize        = errors.New("protocolsession: sequenced message size differs from its reservation")
	ErrSealerSequence       = errors.New("protocolsession: envelope sealer returned a different sequence")
	ErrSealingReservation   = errors.New("protocolsession: reserved delivery lost sealing authority")
	ErrOutboundReplayPermit = errors.New("protocolsession: sender response admission returned no replay permit")
)

// FrameChannel is defined where it is consumed. Retaining the parent Frame and
// ChannelState types keeps transport adapters wire-neutral without importing the
// old business sessions into this runtime.
type FrameChannel interface {
	Send(ctx context.Context, frame framechannel.Frame) error
	SendTerminal(ctx context.Context, frame framechannel.Frame) error
	Recv() <-chan framechannel.Frame
	State() framechannel.ChannelState
	Close() error
}

// OutboundEnvelopeSealer is stateful and belongs exclusively to SessionWriter.
// NextSequence must preflight nonce acquisition and every other fallible input
// needed by Seal. This lets the writer validate operation lifecycle after
// preflight but before consuming a sequence or touching the transport.
type OutboundEnvelopeSealer interface {
	NextSequence() (uint64, error)
	Seal(plaintext []byte) (SealedEnvelope, error)
}

type sequencedMessageBuilder func(sequence uint64) (Message, error)

type messageClass uint8

const (
	classControl messageClass = iota
	classData
	classTerminal
)

type queuedMessage struct {
	message                       Message
	plaintext                     []byte
	builder                       sequencedMessageBuilder
	class                         messageClass
	replay                        OutboundReplayPermit
	authority                     OutboundOperationPermit
	authenticatedViolationHandler func(AuthenticatedOperationViolation)
	pin                           *outboundAdmissionPin
	continuation                  *operationContinuationReservation
	frameSize                     int
	result                        *deliveryResult
}

func (message *queuedMessage) releasePin() {
	if message == nil {
		return
	}
	message.pin.release()
	message.pin = nil
}

func (message *queuedMessage) settleContinuation(commit bool) {
	if message == nil {
		return
	}
	if commit {
		message.continuation.commit()
	} else {
		message.continuation.rollback()
	}
	message.continuation = nil
}

type writerState uint8

const (
	writerAccepting writerState = iota
	writerTerminalAccepted
	writerStopped
)

type queueUsage struct {
	frames int
	bytes  int
}

type SessionWriter struct {
	channel FrameChannel
	sealer  OutboundEnvelopeSealer
	policy  OutboundMessagePolicy

	control  chan *queuedMessage
	data     chan *queuedMessage
	terminal chan *queuedMessage

	mu           sync.Mutex
	state        writerState
	controlUsage queueUsage
	dataUsage    queueUsage

	started atomic.Bool
	done    chan struct{}
	runErr  error
}

func NewSessionWriter(
	channel FrameChannel,
	sealer OutboundEnvelopeSealer,
	policy OutboundMessagePolicy,
) (*SessionWriter, error) {
	if channel == nil || sealer == nil || policy == nil {
		return nil, ErrNilRuntimeDependency
	}
	return &SessionWriter{
		channel:  channel,
		sealer:   sealer,
		policy:   policy,
		control:  make(chan *queuedMessage, ControlQueueFrameLimit),
		data:     make(chan *queuedMessage, DataQueueFrameLimit),
		terminal: make(chan *queuedMessage, 1),
		done:     make(chan struct{}),
	}, nil
}

func (writer *SessionWriter) TryControl(message Message) (SendReceipt, error) {
	return writer.tryControlWithViolationHandler(
		message,
		OutboundReplayPermit{},
		OutboundOperationPermit{},
		nil,
	)
}

// TryControlObservingAuthenticatedViolations installs the exact-generation
// observer after operation admission but before the writer seals or exposes the
// request to the transport. The peer therefore cannot answer in the gap between
// generation creation and observer registration.
func (writer *SessionWriter) TryControlObservingAuthenticatedViolations(
	message Message,
	handler func(AuthenticatedOperationViolation),
) (SendReceipt, error) {
	if handler == nil {
		return SendReceipt{}, ErrAuthenticatedOperationObserver
	}
	if !message.kind.isRequest() {
		return SendReceipt{}, ErrMessageClass
	}
	return writer.tryControlWithViolationHandler(
		message,
		OutboundReplayPermit{},
		OutboundOperationPermit{},
		handler,
	)
}

func (writer *SessionWriter) TryAuthorizedControl(
	message Message,
	authority OutboundOperationPermit,
) (SendReceipt, error) {
	return writer.tryControl(message, OutboundReplayPermit{}, authority)
}

// TryControlReplay queues the exact initial receiver request authorized by an
// ambiguous settled send before dependent controls on a replacement lane.
func (writer *SessionWriter) TryControlReplay(
	message Message,
	permit OutboundReplayPermit,
) (SendReceipt, error) {
	return writer.tryControl(message, permit, OutboundOperationPermit{})
}

func (writer *SessionWriter) tryControl(
	message Message,
	permit OutboundReplayPermit,
	authority OutboundOperationPermit,
) (SendReceipt, error) {
	return writer.tryControlWithViolationHandler(message, permit, authority, nil)
}

func (writer *SessionWriter) tryControlWithViolationHandler(
	message Message,
	permit OutboundReplayPermit,
	authority OutboundOperationPermit,
	handler func(AuthenticatedOperationViolation),
) (SendReceipt, error) {
	if message.IsData() || message.kind == MessageSessionTerminal {
		return SendReceipt{}, ErrMessageClass
	}
	if writer.policy.OutboundDirection() == DirectionSenderToReceiver &&
		(isOperationControl(message.kind) || message.kind == MessageLaneAttach) {
		return SendReceipt{}, ErrSequenceBoundControl
	}
	item, err := prepareQueuedMessage(message, classControl)
	if err != nil {
		return SendReceipt{}, err
	}
	item.replay = permit
	item.authority = authority
	item.authenticatedViolationHandler = handler
	if message.kind == MessageCancel && writer.policy.OutboundDirection() == DirectionReceiverToSender {
		return writer.enqueueCancellation(item)
	}
	if message.kind == MessagePeerCandidate {
		return writer.enqueueContinuation(item)
	}
	return writer.enqueue(item)
}

func (writer *SessionWriter) enqueueCancellation(item *queuedMessage) (SendReceipt, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.state == writerTerminalAccepted {
		return SendReceipt{}, ErrWriterTerminal
	}
	if writer.state == writerStopped {
		return SendReceipt{}, ErrWriterStopped
	}
	if writer.controlUsage.frames == ControlQueueFrameLimit ||
		item.frameSize > ControlQueueByteLimit-writer.controlUsage.bytes {
		return SendReceipt{}, ErrControlQueueFull
	}
	admission, err := writer.policy.AdmitOutbound(item.message, item.authority)
	if err != nil {
		return SendReceipt{}, err
	}
	if admission.Disposition == OperationDrop {
		admission.pin.release()
		item.result.complete(SendOutcomeDropped, admission.Replay, false, nil)
		return item.result.receipt(), nil
	}
	if admission.Disposition != OperationDeliver || admission.Replay.IsZero() {
		admission.pin.release()
		return SendReceipt{}, ErrOutboundReplayPermit
	}
	item.replay = admission.Replay
	item.pin = admission.pin
	if !item.result.admitBeforeQueue(admission) {
		item.releasePin()
		return SendReceipt{}, ErrWriterStopped
	}
	writer.controlUsage.frames++
	writer.controlUsage.bytes += item.frameSize
	writer.control <- item
	return item.result.receipt(), nil
}

func (writer *SessionWriter) enqueueContinuation(item *queuedMessage) (SendReceipt, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.state == writerTerminalAccepted {
		return SendReceipt{}, ErrWriterTerminal
	}
	if writer.state == writerStopped {
		return SendReceipt{}, ErrWriterStopped
	}
	if writer.controlUsage.frames == ControlQueueFrameLimit ||
		item.frameSize > ControlQueueByteLimit-writer.controlUsage.bytes {
		return SendReceipt{}, ErrControlQueueFull
	}
	var admission OutboundAdmission
	var err error
	if !item.replay.IsZero() {
		admission, err = writer.policy.AcceptOutboundReplay(item.message, item.replay)
	} else {
		admission, err = writer.policy.AdmitOutbound(item.message, item.authority)
	}
	if err != nil {
		admission.continuation.rollback()
		admission.pin.release()
		return SendReceipt{}, err
	}
	if admission.Disposition == OperationDrop {
		admission.continuation.rollback()
		admission.pin.release()
		item.result.complete(SendOutcomeDropped, admission.Replay, false, nil)
		return item.result.receipt(), nil
	}
	if admission.Disposition != OperationDeliver ||
		(writer.policy.OutboundDirection() == DirectionSenderToReceiver && admission.Replay.IsZero()) {
		admission.continuation.rollback()
		admission.pin.release()
		return SendReceipt{}, ErrOutboundReplayPermit
	}
	item.replay = admission.Replay
	item.pin = admission.pin
	item.continuation = admission.continuation
	if !item.result.reserveBeforeQueue(admission) {
		item.settleContinuation(false)
		item.releasePin()
		return SendReceipt{}, ErrWriterStopped
	}
	writer.controlUsage.frames++
	writer.controlUsage.bytes += item.frameSize
	writer.control <- item
	return item.result.receipt(), nil
}

// TryControlWithSequence reserves the exact final plaintext size, then invokes
// builder in the writer goroutine with the sealer's next sequence. This is the
// only safe point to add a sender-control signature whose preimage binds it.
func (writer *SessionWriter) tryControlWithSequence(
	plaintextBytes int,
	builder sequencedMessageBuilder,
) (SendReceipt, error) {
	if writer.policy.OutboundDirection() == DirectionSenderToReceiver {
		return SendReceipt{}, ErrSequenceBoundControl
	}
	item, err := prepareSequencedMessage(plaintextBytes, classControl, builder)
	if err != nil {
		return SendReceipt{}, err
	}
	return writer.enqueue(item)
}

// TrySenderControl is the only sender-to-receiver control admission path. The
// prepared callback signs inside the writer goroutine with the exact sequence
// read immediately before envelope sealing.
func (writer *SessionWriter) TrySenderControl(control PreparedSenderControl) (SendReceipt, error) {
	return writer.trySenderControl(control, OutboundReplayPermit{}, OutboundOperationPermit{})
}

func (writer *SessionWriter) TryAuthorizedSenderControl(
	control PreparedSenderControl,
	authority OutboundOperationPermit,
) (SendReceipt, error) {
	return writer.trySenderControl(control, OutboundReplayPermit{}, authority)
}

// TrySenderControlReplay rebuilds one previously attempted semantic response
// with this writer's lane binding and sequence. Operation policy admits only an
// exact permit for the same operation generation and canonical fingerprint.
func (writer *SessionWriter) TrySenderControlReplay(
	control PreparedSenderControl,
	permit OutboundReplayPermit,
) (SendReceipt, error) {
	return writer.trySenderControl(control, permit, OutboundOperationPermit{})
}

func (writer *SessionWriter) trySenderControl(
	control PreparedSenderControl,
	permit OutboundReplayPermit,
	authority OutboundOperationPermit,
) (SendReceipt, error) {
	if writer.policy.OutboundDirection() != DirectionSenderToReceiver || control.build == nil || control.plaintextBytes <= 0 {
		return SendReceipt{}, ErrSequenceBoundControl
	}
	class := classControl
	if control.kind == MessageSessionTerminal {
		class = classTerminal
	}
	item, err := prepareSequencedMessage(control.plaintextBytes, class, control.build)
	if err != nil {
		return SendReceipt{}, err
	}
	item.replay = permit
	item.authority = authority
	if class == classTerminal {
		return writer.acceptTerminal(item)
	}
	item.message = control.intent
	if control.kind == MessagePeerCandidate {
		return writer.enqueueContinuation(item)
	}
	return writer.enqueue(item)
}

func (writer *SessionWriter) TryData(message Message) (SendReceipt, error) {
	return writer.tryData(message, OutboundReplayPermit{}, OutboundOperationPermit{})
}

func (writer *SessionWriter) TryAuthorizedData(
	message Message,
	authority OutboundOperationPermit,
) (SendReceipt, error) {
	return writer.tryData(message, OutboundReplayPermit{}, authority)
}

// TryDataReplay carries the identical authenticated operation fragment over a
// replacement lane after an ambiguous or proven pre-transport failure.
func (writer *SessionWriter) TryDataReplay(
	message Message,
	permit OutboundReplayPermit,
) (SendReceipt, error) {
	return writer.tryData(message, permit, OutboundOperationPermit{})
}

func (writer *SessionWriter) tryData(
	message Message,
	permit OutboundReplayPermit,
	authority OutboundOperationPermit,
) (SendReceipt, error) {
	if !message.IsData() {
		return SendReceipt{}, ErrMessageClass
	}
	item, err := prepareQueuedMessage(message, classData)
	if err != nil {
		return SendReceipt{}, err
	}
	item.replay = permit
	item.authority = authority
	return writer.enqueue(item)
}

func (writer *SessionWriter) enqueue(item *queuedMessage) (SendReceipt, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.state == writerTerminalAccepted {
		return SendReceipt{}, ErrWriterTerminal
	}
	if writer.state == writerStopped {
		return SendReceipt{}, ErrWriterStopped
	}
	queue := writer.control
	usage := &writer.controlUsage
	frameLimit := ControlQueueFrameLimit
	byteLimit := ControlQueueByteLimit
	queueError := ErrControlQueueFull
	if item.class == classData {
		queue = writer.data
		usage = &writer.dataUsage
		frameLimit = DataQueueFrameLimit
		byteLimit = DataQueueByteLimit
		queueError = ErrDataQueueFull
	}
	if usage.frames == frameLimit || item.frameSize > byteLimit-usage.bytes {
		return SendReceipt{}, queueError
	}
	usage.frames++
	usage.bytes += item.frameSize
	queue <- item
	return item.result.receipt(), nil
}

func (writer *SessionWriter) acceptTerminal(item *queuedMessage) (SendReceipt, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.state == writerTerminalAccepted {
		return SendReceipt{}, ErrWriterTerminal
	}
	if writer.state == writerStopped {
		return SendReceipt{}, ErrWriterStopped
	}
	if err := writer.policy.AcceptOutboundTerminal(); err != nil {
		return SendReceipt{}, err
	}
	if !item.result.admitBeforeQueue(OutboundAdmission{Disposition: OperationDeliver}) {
		return SendReceipt{}, ErrWriterStopped
	}

	writer.state = writerTerminalAccepted
	writer.failQueuedLocked(writer.control, &writer.controlUsage, ErrWriterTerminal)
	writer.failQueuedLocked(writer.data, &writer.dataUsage, ErrWriterTerminal)
	writer.terminal <- item
	return item.result.receipt(), nil
}

func prepareQueuedMessage(message Message, class messageClass) (*queuedMessage, error) {
	plaintext, err := EncodeMessage(message)
	if err != nil {
		return nil, err
	}
	frameSize, err := EnvelopeFrameSize(len(plaintext))
	if err != nil {
		return nil, err
	}
	return &queuedMessage{
		message: message, plaintext: plaintext, class: class,
		frameSize: frameSize, result: newDeliveryResult(),
	}, nil
}

func prepareSequencedMessage(
	plaintextBytes int,
	class messageClass,
	builder sequencedMessageBuilder,
) (*queuedMessage, error) {
	if builder == nil || plaintextBytes <= 0 {
		return nil, ErrSequencedBuild
	}
	frameSize, err := EnvelopeFrameSize(plaintextBytes)
	if err != nil {
		return nil, err
	}
	return &queuedMessage{
		builder: builder, class: class, frameSize: frameSize, result: newDeliveryResult(),
	}, nil
}
