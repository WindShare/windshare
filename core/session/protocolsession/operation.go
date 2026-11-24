package protocolsession

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

// Role determines, rather than merely documents, the only directions a runtime
// may receive and send. This prevents accidentally wiring two receiver halves.
type Role uint8

const (
	RoleReceiver Role = iota
	RoleSender
)

func (role Role) InboundDirection() (Direction, error) {
	switch role {
	case RoleReceiver:
		return DirectionSenderToReceiver, nil
	case RoleSender:
		return DirectionReceiverToSender, nil
	default:
		return 0, ErrInvalidRole
	}
}

func (role Role) OutboundDirection() (Direction, error) {
	inbound, err := role.InboundDirection()
	if err != nil {
		return 0, err
	}
	if inbound == DirectionReceiverToSender {
		return DirectionSenderToReceiver, nil
	}
	return DirectionReceiverToSender, nil
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value Role) DiagnosticName() string {
	switch value {
	case RoleReceiver:
		return "receiver"
	case RoleSender:
		return "sender"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

const (
	// OperationReplayWindow is the width (in operation IDs) of the sender's
	// sliding duplicate-detection bitmap, accommodating cross-lane reordering
	// without per-operation wall-clock tombstone retention. 65,536 IDs occupy
	// 8 KiB per session and cover >65 seconds at 1,000 operations/sec.
	OperationReplayWindow = 65536

	ControlQueueFrameLimit    = 256
	ControlQueueByteLimit     = 4 * 1024 * 1024
	DataQueueFrameLimit       = 1024
	DataQueueByteLimit        = 64 * 1024 * 1024
	MaximumDataBurst          = 8
	MaximumControlBurst       = 8
	RouterControlQueueLimit   = 256
	RouterDataQueueLimit      = 1024
	RouterMaximumControlBurst = 8
)

var (
	ErrInvalidRole           = errors.New("protocolsession: invalid role")
	ErrInvalidDirection      = errors.New("protocolsession: message kind is invalid for direction")
	ErrUnknownOperation      = errors.New("protocolsession: message refers to an unknown operation")
	ErrOperationIDReused     = errors.New("protocolsession: operation identity was reused")
	ErrUnexpectedOperation   = errors.New("protocolsession: message is invalid for the operation")
	ErrConflictingFinal      = errors.New("protocolsession: operation final conflicts with its tombstone")
	ErrSessionTerminated     = errors.New("protocolsession: session is terminal")
	ErrActiveOperationBudget = errors.New("protocolsession: active operation budget exhausted")
	ErrNilRuntimeDependency  = errors.New("protocolsession: runtime dependency must not be nil")
	ErrRouterControlFull     = errors.New("protocolsession: inbound control queue is full")
	ErrRouterDataFull        = errors.New("protocolsession: inbound data queue is full")
	ErrHandlerRegistered     = errors.New("protocolsession: message kind already has a handler")
	ErrHandlerMissing        = errors.New("protocolsession: message kind has no handler")
	ErrControlQueueFull      = errors.New("protocolsession: control queue capacity exceeded")
	ErrDataQueueFull         = errors.New("protocolsession: data queue capacity exceeded")
	ErrWriterTerminal        = errors.New("protocolsession: writer accepted a terminal message")
	ErrWriterStopped         = errors.New("protocolsession: writer is stopped")
	ErrMessageClass          = errors.New("protocolsession: message submitted to the wrong writer queue")
	ErrEnvelopeRejected      = errors.New("protocolsession: inbound envelope rejected")
	ErrInboundSemantic       = errors.New("protocolsession: inbound message semantic validation failed")
	ErrInboundRejected       = errors.New("protocolsession: inbound message rejected")
	ErrPeerSessionTerminal   = errors.New("protocolsession: peer ended the session")
	ErrNotSent               = errors.New("protocolsession: message was not handed to transport")
	ErrOperationSuppressed   = errors.New("protocolsession: outbound message suppressed for retired or duplicate operation")
)

// IsOperationCapacityError identifies a retryable refusal before any operation
// authority or wire delivery exists.
func IsOperationCapacityError(err error) bool {
	return errors.Is(err, ErrActiveOperationBudget)
}

// SendEvidence expresses cross-lane transport proof independently of error
// aggregation so joined per-lane failures cannot conflate one pre-transport
// refusal with another lane's uncertain acceptance.
type SendEvidence uint8

const (
	// SendUncertain is the conservative zero value: transport ownership was not
	// proved absent, so callers must retain peer-visible recovery state.
	SendUncertain SendEvidence = iota
	SendDelivered
	SendNotSent
)

type notSentError struct{ cause error }

func (err *notSentError) Error() string { return err.cause.Error() }
func (err *notSentError) Unwrap() error { return err.cause }
func (err *notSentError) Is(target error) bool {
	return target == ErrNotSent
}

type uncertainSendError struct{ cause error }

func (err *uncertainSendError) Error() string { return err.cause.Error() }
func (err *uncertainSendError) Unwrap() error { return err.cause }

// MarkNotSent tags an error proved to have failed before transport acquired the frame.
func MarkNotSent(cause error) error {
	if cause == nil {
		return ErrNotSent
	}
	if _, uncertain := errors.AsType[*uncertainSendError](cause); uncertain {
		return cause
	}
	if errors.Is(cause, ErrNotSent) {
		return cause
	}
	return &notSentError{cause: cause}
}

// MarkSendUncertain marks an aggregated send error where at least one attempt
// may have reached transport. It removes per-attempt ErrNotSent wrappers so
// errors.Is(err, ErrNotSent) stays false and guards IsNotSent under later joins
// while preserving every underlying cause.
func MarkSendUncertain(cause error) error {
	if cause == nil {
		return nil
	}
	cleaned, _ := stripNotSent(cause)
	if cleaned == nil {
		cleaned = cause
	}
	if _, ok := errors.AsType[*uncertainSendError](cleaned); ok {
		return cleaned
	}
	return &uncertainSendError{cause: cleaned}
}

func stripNotSent(err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	switch node := err.(type) { //nolint:errorlint // Inspect one error-tree node at a time so non-notSent causes stay intact.
	case *notSentError:
		cleaned, _ := stripNotSent(node.cause)
		return cleaned, true
	case interface{ Unwrap() []error }:
		children := node.Unwrap()
		cleaned := make([]error, 0, len(children))
		changed := false
		for _, child := range children {
			stripped, childChanged := stripNotSent(child)
			changed = changed || childChanged
			if stripped != nil {
				cleaned = append(cleaned, stripped)
			}
		}
		if !changed {
			return err, false
		}
		return errors.Join(cleaned...), true
	}
	if err == ErrNotSent { //nolint:errorlint // Remove only bare ErrNotSent leaves; keep every wrapped transport cause.
		return nil, true
	}
	return err, false
}

// IsNotSent reports whether the frame is proved never to have reached the transport.
func IsNotSent(err error) bool {
	if _, uncertain := errors.AsType[*uncertainSendError](err); uncertain {
		return false
	}
	return errors.Is(err, ErrNotSent) || errors.Is(err, ErrOperationSuppressed)
}

// FrameChannel is defined where it is consumed. Retaining the parent Frame and
// ChannelState types keeps transport adapters wire-neutral.
type FrameChannel interface {
	Send(ctx context.Context, frame framechannel.Frame) error
	SendTerminal(ctx context.Context, frame framechannel.Frame) error
	Recv() <-chan framechannel.Frame
	State() framechannel.ChannelState
	Close() error
}

type OperationLimits struct {
	MaxActive int
	// MaxRecentTombstones bounds the FIFO cache of recently retired operation
	// metadata retained for cross-lane duplicate/conflict checks. Overflow evicts
	// the oldest unpinned tombstone immediately instead of stalling admission.
	MaxRecentTombstones int
}

// RouterLimits permits lower local admission limits without changing wire limits.
type RouterLimits struct {
	ControlFrames int
	DataFrames    int
}

var DefaultRouterLimits = RouterLimits{
	ControlFrames: RouterControlQueueLimit,
	DataFrames:    RouterDataQueueLimit,
}

// OperationDisposition reports whether an authenticated message is delivered,
// idempotently dropped, terminates the session, or fell behind the replay window.
type OperationDisposition uint8

const (
	OperationDeliver OperationDisposition = iota
	OperationDrop
	OperationSessionTerminal
	OperationBelowReplayWindow
)

// OperationState is the read-only authority query backing an explicit Operation handle.
type OperationState interface {
	Active(OperationID) bool
	Current(OperationID) bool
	RequestKind(OperationID) (MessageKind, bool)
	PeerAttemptBinding(OperationID) (PeerAttemptBinding, bool)
}

// Operation is an explicit, comparable handle passed to message handlers and
// outbound response calls, replacing implicit context.Value capability smuggling.
// Because OperationID is strictly monotonic per session, the ID itself uniquely
// identifies the operation without a separate generation token.
type Operation struct {
	id    OperationID
	state OperationState
}

func NewOperation(id OperationID, state OperationState) Operation {
	return Operation{id: id, state: state}
}

func (op Operation) ID() OperationID           { return op.id }
func (op Operation) IsZero() bool              { return op.id.IsZero() || op.state == nil }
func (op Operation) Same(other Operation) bool { return !op.IsZero() && op == other }
func (op Operation) IsActive() bool            { return !op.IsZero() && op.state.Active(op.id) }
func (op Operation) IsCurrent() bool           { return !op.IsZero() && op.state.Current(op.id) }

func (op Operation) RequestKind() (MessageKind, bool) {
	if op.IsZero() {
		return 0, false
	}
	return op.state.RequestKind(op.id)
}

func (op Operation) PeerAttemptBinding() (PeerAttemptBinding, bool) {
	if op.IsZero() {
		return PeerAttemptBinding{}, false
	}
	return op.state.PeerAttemptBinding(op.id)
}

// MessageHandler consumes authenticated messages with an explicit Operation handle.
// Handlers should admit work into their own bounded queue and return promptly.
type MessageHandler interface {
	HandleMessage(context.Context, Operation, Message) error
}

type MessageHandlerFunc func(context.Context, Operation, Message) error

func (f MessageHandlerFunc) HandleMessage(ctx context.Context, op Operation, message Message) error {
	return f(ctx, op, message)
}

// PeerSessionTerminalError retains the authenticated terminal message for callers
// that decode diagnostics while still supporting errors.Is classification.
type PeerSessionTerminalError struct {
	message Message
}

func NewPeerSessionTerminalError(message Message) *PeerSessionTerminalError {
	return &PeerSessionTerminalError{message: message}
}

func (terminal *PeerSessionTerminalError) Error() string { return ErrPeerSessionTerminal.Error() }
func (terminal *PeerSessionTerminalError) Unwrap() error { return ErrPeerSessionTerminal }
func (terminal *PeerSessionTerminalError) Message() Message {
	return terminal.message
}

func (terminal *PeerSessionTerminalError) Terminal() (SessionTerminal, error) {
	if terminal == nil || terminal.message.Kind() != MessageSessionTerminal {
		return SessionTerminal{}, ErrInvalidSessionTerminal
	}
	return DecodeSessionTerminal(terminal.message.Body())
}

func MessageAllowedForOperation(requestKind, messageKind MessageKind) bool {
	if messageKind == MessageOperationError || messageKind == MessageOperationRetry {
		return true
	}
	switch requestKind {
	case MessageListChildren:
		return messageKind == MessageScanProgress || messageKind == MessageCatalogResult
	case MessageOpenRevisions:
		return messageKind == MessageOpenResults
	case MessageRenewLease:
		return messageKind == MessageLeaseResult
	case MessageReleaseLease:
		return messageKind == MessageOperationComplete
	case MessageRequestBlocks:
		return messageKind == MessageBlockFragment || messageKind == MessageOperationComplete
	case MessageLaneAttach:
		return messageKind == MessageLaneAttach
	case MessagePeerOffer:
		return messageKind == MessagePeerAnswer || messageKind == MessagePeerCandidate
	default:
		return false
	}
}

func ValidateKindDirection(direction Direction, kind MessageKind) error {
	if !kind.Valid() {
		return ErrUnknownMessageKind
	}
	switch direction {
	case DirectionReceiverToSender:
		if kind.IsRequest() || kind == MessageCancel || kind == MessageOperationError || kind == MessageLaneAttach ||
			kind == MessagePeerCandidate || kind.IsSessionControl() {
			return nil
		}
	case DirectionSenderToReceiver:
		switch kind {
		case MessageCatalogResult, MessageOpenResults, MessageBlockFragment,
			MessageOperationError, MessageSessionTerminal, MessageLaneAttach,
			MessageScanProgress, MessageOperationComplete, MessageLeaseResult,
			MessagePeerAnswer, MessagePeerCandidate, MessagePeerPathControl,
			MessageTURNControl, MessageOperationRetry:
			return nil
		}
	}
	return fmt.Errorf("%w: direction=%d kind=%d", ErrInvalidDirection, direction, kind)
}
