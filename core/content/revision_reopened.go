package content

// RevisionReopenedError carries the authenticated current descriptor when an
// existing read cannot migrate to it without admitting the output again.
type RevisionReopenedError struct {
	current    FileRevisionDescriptor
	comparison RevisionComparisonEvidence
}

func NewRevisionReopenedError(previous, current FileRevisionDescriptor) *RevisionReopenedError {
	return &RevisionReopenedError{current: current, comparison: CompareRevisions(previous, current)}
}

func (e *RevisionReopenedError) Current() FileRevisionDescriptor        { return e.current }
func (e *RevisionReopenedError) Comparison() RevisionComparisonEvidence { return e.comparison }
func (e *RevisionReopenedError) Error() string                          { return e.Unwrap().Error() }

func (e *RevisionReopenedError) Unwrap() error {
	if e.comparison.Comparison == RevisionComparisonMismatch {
		return ErrRevisionStale
	}
	return ErrRevisionReopened
}
