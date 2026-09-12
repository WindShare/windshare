package protocolsession

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/framechannel"
)

// ResponseSendEvidence summarizes every physical attempt, independently of why
// the caller returned or whether cleanup succeeded.
type ResponseSendEvidence uint8

const (
	ResponseSendEvidenceUninitialized ResponseSendEvidence = iota
	ResponseSendEvidenceDefinitelyNotSent
	ResponseSendEvidenceUncertain
	ResponseSendEvidenceTransportConfirmed
)

type SendAttemptEnd uint8

const (
	SendAttemptEndUninitialized SendAttemptEnd = iota
	SendAttemptEndRejectedBeforeReceipt
	SendAttemptEndSettled
	SendAttemptEndWaitingEnded
)

type ResponseSendEnd uint8

const (
	ResponseSendEndUninitialized ResponseSendEnd = iota
	ResponseSendEndPreparationFailed
	ResponseSendEndRouteUnavailable
	ResponseSendEndAuthorityUnavailable
	ResponseSendEndTransportConfirmed
	ResponseSendEndPolicySuppressed
	ResponseSendEndCallerCanceled
	ResponseSendEndDeadlineExceeded
	ResponseSendEndRuntimeStopped
	ResponseSendEndRetryDisallowed
	ResponseSendEndNoUsableLane
	ResponseSendEndAttemptsExhausted
	ResponseSendEndAuthorityLost
	ResponseSendEndInvalidReceipt
)

// SendAttemptCause preserves the reason for an earlier failure even when a
// later retry succeeds and the response returns no operational error.
type SendAttemptCauseKind uint8

const (
	SendAttemptCauseNone SendAttemptCauseKind = iota
	SendAttemptCauseCanceled
	SendAttemptCauseDeadline
	SendAttemptCauseControlQueueFull
	SendAttemptCauseDataQueueFull
	SendAttemptCauseWriterStopped
	SendAttemptCauseWriterTerminal
	SendAttemptCauseTransportFailure
	SendAttemptCausePreparationFailure
	SendAttemptCauseAdmissionFailure
)

const MaximumSendAttemptCauseDetailBytes = 256

type SendAttemptCause struct {
	kind      SendAttemptCauseKind
	detail    string
	truncated bool
}

func (cause SendAttemptCause) Kind() SendAttemptCauseKind { return cause.kind }
func (cause SendAttemptCause) Detail() string             { return cause.detail }
func (cause SendAttemptCause) Truncated() bool            { return cause.truncated }

func newSendAttemptCause(err error, fallback SendAttemptCauseKind) SendAttemptCause {
	if err == nil {
		return SendAttemptCause{}
	}
	kind := fallback
	switch {
	case errors.Is(err, context.Canceled):
		kind = SendAttemptCauseCanceled
	case errors.Is(err, context.DeadlineExceeded):
		kind = SendAttemptCauseDeadline
	case errors.Is(err, ErrControlQueueFull):
		kind = SendAttemptCauseControlQueueFull
	case errors.Is(err, ErrDataQueueFull):
		kind = SendAttemptCauseDataQueueFull
	case errors.Is(err, ErrWriterStopped):
		kind = SendAttemptCauseWriterStopped
	case errors.Is(err, ErrWriterTerminal):
		kind = SendAttemptCauseWriterTerminal
	}
	return newSendAttemptCauseText(kind, err.Error())
}

func newSendAttemptCauseText(kind SendAttemptCauseKind, detail string) SendAttemptCause {
	detail = strings.ToValidUTF8(detail, "\uFFFD")
	truncated := len(detail) > MaximumSendAttemptCauseDetailBytes
	if truncated {
		detail = detail[:MaximumSendAttemptCauseDetailBytes]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	// Clone after bounding so a short prefix never retains an arbitrary error's
	// large backing string or any of the authority reachable through that error.
	return SendAttemptCause{kind: kind, detail: strings.Clone(detail), truncated: truncated}
}

type SendCleanupKind uint8

const (
	SendCleanupNone SendCleanupKind = iota
	SendCleanupRouteReleased
	SendCleanupOperationRetired
	SendCleanupFailed
)

var (
	ErrSendAttemptSnapshot = errors.New("protocol session send attempt snapshot is invalid")
	ErrResponseSendResult  = errors.New("protocol session response send result is invalid")
)

// SendAttemptIdentity pins the lane before execution. Route migration must not
// rewrite the identity of an attempt whose transport already owns the frame.
type SendAttemptIdentity struct {
	ResponseSequence uint64
	AttemptSequence  uint32
	LaneID           uint32
	LaneEpoch        uint32
}

func (id SendAttemptIdentity) valid() bool {
	return id.ResponseSequence != 0 && id.AttemptSequence != 0 &&
		id.AttemptSequence <= DefaultMaxLogicalLanes && id.LaneID != 0
}

// SendAttemptSnapshot deliberately contains no operation, replay, lease, error
// or body references. Copies remain factual values after execution releases its
// authority and after a pending receipt eventually settles.
type SendAttemptSnapshot struct {
	identity             SendAttemptIdentity
	policyAdmitted       bool
	outcome              SendOutcome
	transportDisposition framechannel.SendDisposition
	end                  SendAttemptEnd
	cause                SendAttemptCause
}

func NewRejectedSendAttempt(id SendAttemptIdentity, cause error) (SendAttemptSnapshot, error) {
	if !id.valid() {
		return SendAttemptSnapshot{}, ErrSendAttemptSnapshot
	}
	return SendAttemptSnapshot{identity: id, outcome: SendOutcomeDropped, end: SendAttemptEndRejectedBeforeReceipt,
		cause: newSendAttemptCause(cause, SendAttemptCauseAdmissionFailure)}, nil
}

func NewSettledSendAttempt(id SendAttemptIdentity, completion SendCompletion) (SendAttemptSnapshot, error) {
	if !id.valid() || !completion.Settled || !validSettledSendEvidence(completion) {
		return SendAttemptSnapshot{}, ErrSendAttemptSnapshot
	}
	cause := completion.cause
	if cause.Kind() == SendAttemptCauseNone && completion.Err != nil {
		fallback := SendAttemptCausePreparationFailure
		if completion.TransportDisposition != 0 {
			fallback = SendAttemptCauseTransportFailure
		}
		cause = newSendAttemptCause(completion.Err, fallback)
	}
	return SendAttemptSnapshot{
		identity: id, policyAdmitted: completion.Admitted,
		outcome: completion.Outcome, transportDisposition: completion.TransportDisposition,
		end: SendAttemptEndSettled, cause: cause,
	}, nil
}

func validSettledSendEvidence(completion SendCompletion) bool {
	switch completion.Outcome {
	case SendOutcomeTransportConfirmed:
		return completion.TransportDisposition == framechannel.SendAccepted
	case SendOutcomeUnknown:
		return completion.TransportDisposition == framechannel.SendAccepted
	case SendOutcomeDropped:
		return completion.TransportDisposition == 0 ||
			completion.TransportDisposition == framechannel.SendRejected ||
			completion.TransportDisposition == framechannel.SendRetired
	default:
		return false
	}
}

func NewPendingSendAttempt(id SendAttemptIdentity, completion SendCompletion) (SendAttemptSnapshot, error) {
	if !id.valid() || completion.Settled || completion.Outcome != SendOutcomeUnknown ||
		completion.TransportDisposition != 0 {
		return SendAttemptSnapshot{}, ErrSendAttemptSnapshot
	}
	return SendAttemptSnapshot{
		identity: id, policyAdmitted: completion.Admitted,
		outcome: SendOutcomeUnknown, end: SendAttemptEndWaitingEnded,
		cause: newSendAttemptCause(completion.Err, SendAttemptCausePreparationFailure),
	}, nil
}

func (attempt SendAttemptSnapshot) IsZero() bool                  { return attempt.end == SendAttemptEndUninitialized }
func (attempt SendAttemptSnapshot) Identity() SendAttemptIdentity { return attempt.identity }
func (attempt SendAttemptSnapshot) PolicyAdmitted() bool          { return attempt.policyAdmitted }
func (attempt SendAttemptSnapshot) Settled() bool                 { return attempt.end == SendAttemptEndSettled }
func (attempt SendAttemptSnapshot) Outcome() SendOutcome          { return attempt.outcome }
func (attempt SendAttemptSnapshot) TransportDisposition() framechannel.SendDisposition {
	return attempt.transportDisposition
}
func (attempt SendAttemptSnapshot) End() SendAttemptEnd     { return attempt.end }
func (attempt SendAttemptSnapshot) Cause() SendAttemptCause { return attempt.cause }

func (attempt SendAttemptSnapshot) valid() bool {
	if !attempt.identity.valid() {
		return false
	}
	switch attempt.end {
	case SendAttemptEndRejectedBeforeReceipt:
		return !attempt.policyAdmitted && attempt.outcome == SendOutcomeDropped && attempt.transportDisposition == 0
	case SendAttemptEndWaitingEnded:
		return attempt.outcome == SendOutcomeUnknown && attempt.transportDisposition == 0
	case SendAttemptEndSettled:
		return validSettledSendEvidence(SendCompletion{
			Outcome: attempt.outcome, TransportDisposition: attempt.transportDisposition,
		})
	default:
		return false
	}
}

// FoldResponseSendEvidence preserves prior uncertainty even when a later attempt
// is suppressed. Validation continues after confirmation so malformed history
// cannot acquire a successful business meaning through a short circuit.
func FoldResponseSendEvidence(attempts []SendAttemptSnapshot) (ResponseSendEvidence, error) {
	if len(attempts) == 0 || len(attempts) > DefaultMaxLogicalLanes {
		return ResponseSendEvidenceUninitialized, ErrResponseSendResult
	}
	evidence := ResponseSendEvidenceDefinitelyNotSent
	for _, attempt := range attempts {
		if !attempt.valid() {
			return ResponseSendEvidenceUninitialized, ErrSendAttemptSnapshot
		}
		switch attempt.Outcome() {
		case SendOutcomeTransportConfirmed:
			evidence = ResponseSendEvidenceTransportConfirmed
		case SendOutcomeUnknown:
			if evidence != ResponseSendEvidenceTransportConfirmed {
				evidence = ResponseSendEvidenceUncertain
			}
		}
	}
	return evidence, nil
}

// ResponseSendResult owns bounded inline history so neither the caller's input
// slice nor a later receipt settlement can mutate an already returned result.
type ResponseSendResult struct {
	attempts     [DefaultMaxLogicalLanes]SendAttemptSnapshot
	attemptCount uint8
	evidence     ResponseSendEvidence
	end          ResponseSendEnd
	cleanup      SendCleanupKind
}

func NewResponseSendNotStarted(end ResponseSendEnd) (ResponseSendResult, error) {
	switch end {
	case ResponseSendEndPreparationFailed, ResponseSendEndRouteUnavailable,
		ResponseSendEndAuthorityUnavailable, ResponseSendEndCallerCanceled,
		ResponseSendEndDeadlineExceeded, ResponseSendEndRuntimeStopped,
		ResponseSendEndNoUsableLane, ResponseSendEndAuthorityLost:
		return ResponseSendResult{end: end, evidence: ResponseSendEvidenceDefinitelyNotSent}, nil
	default:
		return ResponseSendResult{}, ErrResponseSendResult
	}
}

func NewResponseSendReturned(end ResponseSendEnd, attempts []SendAttemptSnapshot) (ResponseSendResult, error) {
	return newResponseSendResult(end, attempts, false)
}

func NewResponseSendWaitingEnded(end ResponseSendEnd, attempts []SendAttemptSnapshot) (ResponseSendResult, error) {
	switch end {
	case ResponseSendEndCallerCanceled, ResponseSendEndDeadlineExceeded,
		ResponseSendEndRuntimeStopped, ResponseSendEndAuthorityLost:
		return newResponseSendResult(end, attempts, true)
	default:
		return ResponseSendResult{}, ErrResponseSendResult
	}
}

func newResponseSendResult(end ResponseSendEnd, attempts []SendAttemptSnapshot, pending bool) (ResponseSendResult, error) {
	if end < ResponseSendEndTransportConfirmed || end > ResponseSendEndInvalidReceipt {
		return ResponseSendResult{}, ErrResponseSendResult
	}
	evidence, err := FoldResponseSendEvidence(attempts)
	if err != nil {
		return ResponseSendResult{}, err
	}
	responseSequence := attempts[0].Identity().ResponseSequence
	for index, attempt := range attempts {
		identity := attempt.Identity()
		if identity.ResponseSequence != responseSequence || identity.AttemptSequence != uint32(index+1) ||
			(attempt.End() == SendAttemptEndWaitingEnded) != (pending && index == len(attempts)-1) {
			return ResponseSendResult{}, ErrResponseSendResult
		}
	}
	if end == ResponseSendEndTransportConfirmed && evidence != ResponseSendEvidenceTransportConfirmed {
		return ResponseSendResult{}, ErrResponseSendResult
	}
	if end == ResponseSendEndInvalidReceipt {
		// Prior attempts cannot prove what the invalid current invocation did.
		// Keep their history without granting definitely-unsent cleanup authority.
		evidence = ResponseSendEvidenceUninitialized
	}
	result := ResponseSendResult{end: end, evidence: evidence, attemptCount: uint8(len(attempts))}
	copy(result.attempts[:], attempts)
	return result, nil
}

func (result ResponseSendResult) IsZero() bool {
	return result.end == ResponseSendEndUninitialized
}
func (result ResponseSendResult) Started() bool                  { return result.attemptCount != 0 }
func (result ResponseSendResult) Evidence() ResponseSendEvidence { return result.evidence }
func (result ResponseSendResult) End() ResponseSendEnd           { return result.end }
func (result ResponseSendResult) AttemptCount() int              { return int(result.attemptCount) }
func (result ResponseSendResult) Attempt(index int) (SendAttemptSnapshot, bool) {
	if index < 0 || index >= int(result.attemptCount) {
		return SendAttemptSnapshot{}, false
	}
	return result.attempts[index], true
}
func (result ResponseSendResult) PendingAttempt() (SendAttemptIdentity, bool) {
	attempt, exists := result.Attempt(result.AttemptCount() - 1)
	if !exists || attempt.End() != SendAttemptEndWaitingEnded {
		return SendAttemptIdentity{}, false
	}
	return attempt.Identity(), true
}
func (result ResponseSendResult) Cleanup() SendCleanupKind { return result.cleanup }

// WithCleanup cannot change physical evidence. Invalid cleanup values fail
// closed to an uninitialized result instead of creating a diagnostic enum that
// downstream consumers could mistake for a successful cleanup.
func (result ResponseSendResult) WithCleanup(cleanup SendCleanupKind) ResponseSendResult {
	if result.IsZero() || cleanup > SendCleanupFailed {
		return ResponseSendResult{}
	}
	result.cleanup = cleanup
	return result
}
