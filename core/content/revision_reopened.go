package content

// RevisionReopenedError binds an interrupted read to the authenticated descriptor
// offered for a fresh output admission.
type RevisionReopenedError struct {
	previous, current FileRevisionDescriptor
	comparison        RevisionComparisonEvidence
}

func NewRevisionReopenedError(previous, current FileRevisionDescriptor) *RevisionReopenedError {
	return &RevisionReopenedError{previous: previous, current: current, comparison: CompareRevisions(previous, current)}
}

func (e *RevisionReopenedError) Previous() FileRevisionDescriptor       { return e.previous }
func (e *RevisionReopenedError) Current() FileRevisionDescriptor        { return e.current }
func (e *RevisionReopenedError) Comparison() RevisionComparisonEvidence { return e.comparison }
func (e *RevisionReopenedError) Error() string                          { return e.Unwrap().Error() }

func (e *RevisionReopenedError) Unwrap() error {
	if e.comparison.Comparison == RevisionComparisonMismatch {
		return ErrRevisionStale
	}
	return ErrRevisionReopened
}
