package transfer

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type SessionFailureError struct{ cause error }

// IsolatedPermanentSourceFailureError is explicit retirement authority. A raw
// range error is intentionally insufficient because it may be retryable
// transport failure or may have originated in the output sink.
type IsolatedPermanentSourceFailureError struct{ cause error }

// IsolatedFileSourceFailureError authorizes a verified file-local pause while
// preserving retryable source state. It does not authorize retirement.
type IsolatedFileSourceFailureError struct{ cause error }

func NewIsolatedFileSourceFailure(cause error) error {
	if cause == nil {
		cause = errors.New("file source operation failed")
	}
	return &IsolatedFileSourceFailureError{cause: cause}
}

func (failure *IsolatedFileSourceFailureError) Error() string {
	return fmt.Sprintf("transfer isolated file source failure: %v", failure.cause)
}
func (failure *IsolatedFileSourceFailureError) Unwrap() error      { return failure.cause }
func (*IsolatedFileSourceFailureError) IsolatedFileSourceFailure() {}

func NewIsolatedPermanentSourceFailure(cause error) error {
	if cause == nil {
		cause = errors.New("file source failed permanently")
	}
	return &IsolatedPermanentSourceFailureError{cause: cause}
}

func (failure *IsolatedPermanentSourceFailureError) Error() string {
	return fmt.Sprintf("transfer isolated permanent source failure: %v", failure.cause)
}
func (failure *IsolatedPermanentSourceFailureError) Unwrap() error { return failure.cause }
func (failure *IsolatedPermanentSourceFailureError) IsolatedPermanentSourceFailure() {
}
func (*IsolatedPermanentSourceFailureError) IsolatedFileSourceFailure() {}

func NewSessionFailure(cause error) error {
	if cause == nil {
		cause = errors.New("protocol session failed")
	}
	return &SessionFailureError{cause: cause}
}

func (e *SessionFailureError) Error() string   { return fmt.Sprintf("transfer session: %v", e.cause) }
func (e *SessionFailureError) Unwrap() error   { return e.cause }
func (e *SessionFailureError) SessionFailure() {}

func isSessionFailure(err error) bool {
	return inspectLifecycleError(err).jobTerminalSession()
}

func IsSessionFailure(err error) bool { return isSessionFailure(err) }

func isSourceDriftFailure(err error) bool {
	return inspectLifecycleError(err).sourceDrift
}

// JobResourceBudgetError terminates one transfer because a local, bounded
// resource policy was exhausted. It must not be attributed to the peer session.
type JobResourceBudgetError struct{ cause error }

func NewJobResourceBudgetError(cause error) error {
	if cause == nil {
		cause = errors.New("transfer job resource budget exceeded")
	}
	return &JobResourceBudgetError{cause: cause}
}

func (e *JobResourceBudgetError) Error() string {
	return fmt.Sprintf("transfer job resource budget: %v", e.cause)
}
func (e *JobResourceBudgetError) Unwrap() error { return e.cause }
func (e *JobResourceBudgetError) JobFatal()     {}

// JobDependencyContractError is a local collaborator breach, not peer fault.
type JobDependencyContractError struct{ cause error }

func NewJobDependencyContractError(cause error) error {
	if cause == nil {
		cause = errors.New("transfer job dependency contract violated")
	}
	return &JobDependencyContractError{cause: cause}
}

func (e *JobDependencyContractError) Error() string {
	return fmt.Sprintf("transfer job dependency contract: %v", e.cause)
}
func (e *JobDependencyContractError) Unwrap() error { return e.cause }
func (e *JobDependencyContractError) JobFatal()     {}

func isJobFatal(err error) bool {
	inspection := inspectLifecycleError(err)
	return inspection.jobFatal || inspection.exhausted
}

func isJobTerminalError(err error) bool {
	return inspectLifecycleError(err).jobTerminal()
}

type lifecycleErrorInspection struct {
	sessionFailure             bool
	jobFatal                   bool
	jobResourceBudget          bool
	jobDependencyContract      bool
	directoryDiscovery         bool
	demandNotAdmitted          bool
	interrupted                bool
	outputFailure              bool
	explicitOutputPause        bool
	sourceDrift                bool
	invalidatedRevision        bool
	isolatedPermanent          bool
	isolatedFileSource         bool
	outputContract             bool
	directoryAdmissionMismatch bool
	validOutputFault           bool
	fileOutputFault            bool
	nonFileOutputFault         bool
	exhausted                  bool
}

func (inspection lifecycleErrorInspection) jobTerminalSession() bool {
	return inspection.sessionFailure || inspection.exhausted
}

func (inspection lifecycleErrorInspection) jobTerminal() bool {
	return inspection.sessionFailure || inspection.jobFatal || inspection.interrupted || inspection.exhausted
}

func (inspection lifecycleErrorInspection) outputRequiresJobPause(capabilities OutputCapabilities) bool {
	return inspection.explicitOutputPause || inspection.exhausted || !capabilities.FileFailureIsolation
}

func (inspection lifecycleErrorInspection) outputCanContinueAfterFileSettlement(
	capabilities OutputCapabilities,
) bool {
	if !inspection.outputFailure || inspection.jobTerminal() || inspection.explicitOutputPause || inspection.exhausted {
		return false
	}
	if capabilities.FileFailureIsolation {
		return true
	}
	// A ZIP stream cannot promise file isolation statically because an active
	// member may already have changed the archive. At the member-start boundary,
	// however, a purely file-scoped fault can be isolated when Pause proves that
	// this transaction never crossed that boundary.
	return capabilities.Mode == OutputZIPStream &&
		capabilities.ArchiveBoundary == ArchiveFailureAtMemberStart &&
		inspection.fileOutputFault && !inspection.nonFileOutputFault
}

func (inspection lifecycleErrorInspection) retireReason() FileRetireReason {
	if inspection.invalidatedRevision {
		return FileRetireInvalidatedRevision
	}
	if inspection.isolatedPermanent {
		return FileRetireIsolatedPermanentSourceFailure
	}
	return 0
}

// inspectLifecycleError is the sole settlement-path traversal of collaborator
// error graphs. Exact sentinel nodes and typed authorities are recognized while
// the cycle set and frontier budget prevent hostile Unwrap implementations from
// hanging cancellation or durable settlement.
//
//nolint:errorlint // Recursive errors.Is/As cannot provide this walk's cycle or work bounds.
func inspectLifecycleError(root error) lifecycleErrorInspection {
	var inspection lifecycleErrorInspection
	if root == nil {
		return inspection
	}
	pending := []error{root}
	seen := make(map[error]struct{})
	inspected := 0
	budgetOmitted := false
	for len(pending) != 0 && inspected < maxOutputFailureTreeNodes {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current == nil {
			continue
		}
		inspected++
		duplicate, invalid := inspection.visitLifecycleNode(current, seen)
		if invalid {
			inspection.exhausted = true
			return inspection
		}
		if duplicate {
			continue
		}
		remaining := maxOutputFailureTreeNodes - inspected - len(pending)
		var omitted bool
		pending, omitted = appendLifecycleChildren(pending, current, remaining)
		budgetOmitted = budgetOmitted || omitted
	}
	inspection.exhausted = budgetOmitted || len(pending) != 0
	return inspection
}

func (inspection *lifecycleErrorInspection) visitLifecycleNode(
	current error,
	seen map[error]struct{},
) (duplicate bool, invalid bool) {
	currentValue := reflect.ValueOf(current)
	if isNilableErrorValue(currentValue) && currentValue.IsNil() {
		return false, true
	}
	if reflect.TypeOf(current).Comparable() {
		if _, exists := seen[current]; exists {
			return true, false
		}
		seen[current] = struct{}{}
		inspection.acceptComparable(current)
	}
	inspection.acceptLifecycleAuthorities(current)
	return false, false
}

//nolint:errorlint // The bounded walker already exposes each exact graph node.
func (inspection *lifecycleErrorInspection) acceptLifecycleAuthorities(current error) {
	if _, ok := current.(interface{ SessionFailure() }); ok {
		inspection.sessionFailure = true
	}
	if _, ok := current.(interface{ JobFatal() }); ok {
		inspection.jobFatal = true
	}
	if _, ok := current.(*JobResourceBudgetError); ok {
		inspection.jobResourceBudget = true
	}
	if _, ok := current.(*JobDependencyContractError); ok {
		inspection.jobDependencyContract = true
	}
	if _, ok := current.(DirectoryDiscoveryFailure); ok {
		inspection.directoryDiscovery = true
	}
	if _, ok := current.(*demandNotAdmittedError); ok {
		inspection.demandNotAdmitted = true
	}
	if scoped, ok := current.(jobPauseRequirement); ok {
		inspection.outputFailure = true
		inspection.explicitOutputPause = inspection.explicitOutputPause || scoped.RequiresJobPause()
	}
	if _, ok := current.(isolatedPermanentSourceFailure); ok {
		inspection.isolatedPermanent = true
	}
	if _, ok := current.(isolatedFileSourceFailure); ok {
		inspection.isolatedFileSource = true
	}
	inspection.acceptOutputFault(current)
}

//nolint:errorlint // The bounded walker already exposes each exact graph node.
func (inspection *lifecycleErrorInspection) acceptOutputFault(current error) {
	fault, ok := current.(*OutputFault)
	if !ok || fault.Scope() < OutputFaultFile || fault.Scope() > OutputFaultRoot ||
		fault.Code() < OutputFaultStateIO || fault.Code() > OutputFaultContract {
		return
	}
	inspection.validOutputFault = true
	if fault.Scope() == OutputFaultFile {
		inspection.fileOutputFault = true
		return
	}
	inspection.nonFileOutputFault = true
}

//nolint:errorlint // Recursive errors.As cannot provide cycle or work bounds.
func appendLifecycleChildren(pending []error, current error, remaining int) ([]error, bool) {
	switch wrapped := current.(type) {
	case interface{ Unwrap() []error }:
		children := wrapped.Unwrap()
		if remaining <= 0 {
			return pending, len(children) != 0
		}
		if len(children) > remaining {
			return append(pending, children[:remaining]...), true
		}
		return append(pending, children...), false
	case interface{ Unwrap() error }:
		child := wrapped.Unwrap()
		if remaining <= 0 {
			return pending, child != nil
		}
		return append(pending, child), false
	default:
		return pending, false
	}
}

func isNilableErrorValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return true
	default:
		return false
	}
}

//nolint:errorlint // The bounded walker already exposes each exact graph node.
func (inspection *lifecycleErrorInspection) acceptComparable(current error) {
	switch current {
	case context.Canceled, context.DeadlineExceeded:
		inspection.interrupted = true
	case protocolsession.ErrSessionTerminated, protocolsession.ErrPeerSessionTerminal,
		protocolsession.ErrWriterTerminal, protocolsession.ErrWriterStopped, ErrLaneClosed:
		inspection.sessionFailure = true
	case catalog.ErrDirectoryStale, content.ErrRevisionStale, content.ErrSourceDrift:
		inspection.sourceDrift = true
	case content.ErrRevisionDrift, ErrBlockInvalidated:
		inspection.sourceDrift = true
		inspection.invalidatedRevision = true
	case ErrOutputContract:
		inspection.outputContract = true
	case ErrDirectoryAdmissionMismatch:
		inspection.directoryAdmissionMismatch = true
	}
}
