package sessionobs

import "strconv"

// ProtocolOperationCause is deliberately closed and text-free. Raw transport
// errors can contain endpoints or provider details that do not belong in a
// privacy-safe user trace.
type ProtocolOperationCause uint8

const (
	ProtocolOperationCauseNone ProtocolOperationCause = iota
	ProtocolOperationCauseCanceled
	ProtocolOperationCauseDeadline
	ProtocolOperationCauseRuntimeClosed
	ProtocolOperationCauseLaneUnavailable
	ProtocolOperationCauseWriterStopped
	ProtocolOperationCauseOperationClosed
	ProtocolOperationCauseProtocolFailure
	ProtocolOperationCauseSuperseded
)

// IsLifecycle identifies causes that alone cannot establish an operation fault.
// None is included because an empty observation contributes no fault evidence.
// A normal end still requires the terminal owner's evidence.
func (cause ProtocolOperationCause) IsLifecycle() bool {
	return cause == ProtocolOperationCauseNone || cause == ProtocolOperationCauseCanceled ||
		cause == ProtocolOperationCauseOperationClosed || cause == ProtocolOperationCauseSuperseded
}

// Merge prefers failure evidence over lifecycle causes so cancellation cannot
// mask a later authenticated fault or failed cleanup. None contributes no evidence.
// Within either category, the first non-None cause wins; order matters.
func (cause ProtocolOperationCause) Merge(next ProtocolOperationCause) ProtocolOperationCause {
	if cause == ProtocolOperationCauseNone || (cause.IsLifecycle() && !next.IsLifecycle()) {
		return next
	}
	return cause
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value ProtocolOperationCause) DiagnosticName() string {
	switch value {
	case ProtocolOperationCauseNone:
		return "none"
	case ProtocolOperationCauseCanceled:
		return "canceled"
	case ProtocolOperationCauseDeadline:
		return "deadline"
	case ProtocolOperationCauseRuntimeClosed:
		return "runtime_closed"
	case ProtocolOperationCauseLaneUnavailable:
		return "lane_unavailable"
	case ProtocolOperationCauseWriterStopped:
		return "writer_stopped"
	case ProtocolOperationCauseOperationClosed:
		return "operation_closed"
	case ProtocolOperationCauseProtocolFailure:
		return "protocol_failure"
	case ProtocolOperationCauseSuperseded:
		return "superseded"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}
