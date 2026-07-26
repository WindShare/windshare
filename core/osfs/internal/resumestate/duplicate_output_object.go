package resumestate

import (
	"bytes"
	"fmt"
)

// DuplicateOutputObjectDecision binds an unordered pair of persisted records.
// The binding prevents a decision made during one locked namespace scan from
// being replayed against a different record or a later record generation.
type DuplicateOutputObjectDecision struct {
	first            recoveryRecordBinding
	second           recoveryRecordBinding
	quarantineReason QuarantineReason
	bound            bool
}

// ReduceDuplicateOutputObject produces one symmetric decision for two distinct
// selected files that claim the same session-local object identity. It performs
// no I/O; callers must durably install the decision for both records before any
// content operation is allowed to resume.
func ReduceDuplicateOutputObject(
	left BoundFileRecord,
	right BoundFileRecord,
) (DuplicateOutputObjectDecision, error) {
	if !left.valid() || !right.valid() || left.session.namespace != right.session.namespace ||
		left.record.locatorDigest == right.record.locatorDigest ||
		left.record.outputObject != right.record.outputObject {
		return DuplicateOutputObjectDecision{}, fmt.Errorf("%w: duplicate output object records", ErrInvalidState)
	}

	first, second := recoveryBindingFor(left), recoveryBindingFor(right)
	firstDigest, secondDigest := first.locatorDigest, second.locatorDigest
	if bytes.Compare(firstDigest[:], secondDigest[:]) > 0 {
		first, second = second, first
	}
	return DuplicateOutputObjectDecision{
		first: first, second: second,
		quarantineReason: QuarantineOutputObjectDuplicate,
		bound:            true,
	}, nil
}

func (decision DuplicateOutputObjectDecision) QuarantineReason() QuarantineReason {
	return decision.quarantineReason
}

// ApplyDuplicateOutputObjectDecision returns the durable next generation for
// either member of the pair. A member already quarantined remains unchanged so
// recovery can finish the other member after a crash between the two installs.
func ApplyDuplicateOutputObjectDecision(
	bound BoundFileRecord,
	decision DuplicateOutputObjectDecision,
) (BoundFileRecord, error) {
	if !bound.valid() || !decision.valid() {
		return BoundFileRecord{}, fmt.Errorf("%w: duplicate output object decision", ErrInvalidState)
	}
	binding := recoveryBindingFor(bound)
	if binding != decision.first && binding != decision.second {
		return BoundFileRecord{}, fmt.Errorf("%w: duplicate output object record binding", ErrInvalidState)
	}
	if bound.record.phase == FileQuarantined {
		return bound, nil
	}
	return bound.transition(FileTransition{
		Next: FileQuarantined, QuarantineReason: decision.quarantineReason,
	})
}

func (decision DuplicateOutputObjectDecision) valid() bool {
	return decision.bound && decision.quarantineReason == QuarantineOutputObjectDuplicate &&
		decision.first.namespace == decision.second.namespace &&
		decision.first.locatorDigest != decision.second.locatorDigest &&
		decision.first.outputObject == decision.second.outputObject &&
		!decision.first.outputObject.IsZero()
}
