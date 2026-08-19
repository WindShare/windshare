package content

import "errors"

type RevisionComparison uint8

const (
	RevisionComparisonUnknown RevisionComparison = iota
	RevisionComparisonMatch
	RevisionComparisonMismatch
	RevisionComparisonUnavailable
)

type revisionComparisonError struct {
	comparison RevisionComparison
	err        error
}

func (e revisionComparisonError) Error() string                          { return e.err.Error() }
func (e revisionComparisonError) Unwrap() error                          { return e.err }
func (e revisionComparisonError) RevisionComparison() RevisionComparison { return e.comparison }

// WithRevisionComparison adds the provider's evidence conclusion without
// changing the existing typed error chain consumed by the session protocol.
func WithRevisionComparison(err error, comparison RevisionComparison) error {
	if err == nil {
		return nil
	}
	if comparison != RevisionComparisonMismatch && comparison != RevisionComparisonUnavailable {
		return err
	}
	return revisionComparisonError{comparison: comparison, err: err}
}

func RevisionComparisonOf(err error) RevisionComparison {
	if err == nil {
		return RevisionComparisonMatch
	}
	type classified interface {
		RevisionComparison() RevisionComparison
	}
	var marker classified
	if errors.As(err, &marker) {
		comparison := marker.RevisionComparison()
		if comparison == RevisionComparisonMismatch || comparison == RevisionComparisonUnavailable {
			return comparison
		}
	}
	if errors.Is(err, ErrSourceDrift) || errors.Is(err, ErrRevisionStale) {
		return RevisionComparisonMismatch
	}
	return RevisionComparisonUnavailable
}
