package output

import (
	"errors"
	"slices"

	"github.com/windshare/windshare/engine/observation"
)

var ErrBusy = errors.New("output destination or operation is already in use")

// FailureStage describes the application operation that failed, independently
// of the provider's persistence primitives or native error vocabulary.
type FailureStage uint8

const (
	FailureDestinationBinding FailureStage = iota + 1
	FailureInventory
	FailureLookup
	FailureOpen
	FailureCreate
	FailureCheckpoint
	FailureDurability
	FailureClose
)

func (stage FailureStage) Valid() bool {
	return stage >= FailureDestinationBinding && stage <= FailureClose
}

func (stage FailureStage) String() string {
	switch stage {
	case FailureDestinationBinding:
		return "destination_binding"
	case FailureInventory:
		return "inventory"
	case FailureLookup:
		return "lookup"
	case FailureOpen:
		return "open"
	case FailureCreate:
		return "create"
	case FailureCheckpoint:
		return "checkpoint"
	case FailureDurability:
		return "durability"
	case FailureClose:
		return "close"
	default:
		return ""
	}
}

type FailureDiagnostic struct {
	Stage   FailureStage
	Failure observation.Failure
}

func (diagnostic FailureDiagnostic) Valid() bool {
	return diagnostic.Stage.Valid() &&
		(diagnostic.Failure == (observation.Failure{}) || diagnostic.Failure.Valid())
}

// FailureEvidence binds one stage to its exact causes, so cleanup failures can
// be selected without retaining unrelated work from the original error tree.
type FailureEvidence struct {
	Cause      error
	Diagnostic FailureDiagnostic
}

// A concrete envelope makes classification explicit. Providers translate their
// errors here; arbitrary error As methods cannot manufacture application facts.
type failure struct {
	cause    error
	evidence []FailureEvidence
}

func (value *failure) Error() string { return value.cause.Error() }
func (value *failure) Unwrap() error { return value.cause }

func SealFailure(cause error, diagnostic FailureDiagnostic) (error, bool) {
	return SealFailures(cause, []FailureEvidence{{Cause: cause, Diagnostic: diagnostic}})
}

// SealFailures retains the original error while identifying exactly which cause
// belongs to each stage. A close annotation must never claim unrelated work.
func SealFailures(cause error, evidence []FailureEvidence) (error, bool) {
	if cause == nil || len(evidence) == 0 {
		return nil, false
	}
	for _, item := range evidence {
		if item.Cause == nil || !item.Diagnostic.Valid() {
			return nil, false
		}
	}
	return &failure{cause: cause, evidence: slices.Clone(evidence)}, true
}

func DiagnosticFor(cause error) (FailureDiagnostic, bool) {
	//nolint:errorlint
	value, ok := cause.(*failure)
	if !ok || value == nil || len(value.evidence) == 0 {
		return FailureDiagnostic{}, false
	}
	return value.evidence[0].Diagnostic, value.evidence[0].Diagnostic.Valid()
}

// Joined primary and release failures are independent facts. Selecting cleanup
// must never retain an unrelated primary failure or lose a second close failure.
func FailureForStage(cause error, stage FailureStage) error {
	if !stage.Valid() {
		return nil
	}
	budget := maximumFailureNodes
	return failureForStage(cause, stage, 0, &budget)
}

const maximumFailureDepth = 64
const maximumFailureNodes = 1024

func failureForStage(cause error, stage FailureStage, depth int, budget *int) error {
	if cause == nil || depth >= maximumFailureDepth || *budget == 0 {
		return nil
	}
	*budget--
	//nolint:errorlint
	if annotated, ok := cause.(*failure); ok && annotated != nil {
		var matching []error
		for _, item := range annotated.evidence {
			if item.Diagnostic.Stage == stage {
				selected, _ := SealFailure(item.Cause, item.Diagnostic)
				matching = append(matching, selected)
			}
		}
		if len(matching) > 0 {
			return errors.Join(matching...)
		}
	}
	//nolint:errorlint
	switch value := cause.(type) {
	case interface{ Unwrap() []error }:
		var selected []error
		for _, child := range value.Unwrap() {
			if match := failureForStage(child, stage, depth+1, budget); match != nil {
				selected = append(selected, match)
			}
		}
		return errors.Join(selected...)
	case interface{ Unwrap() error }:
		return failureForStage(value.Unwrap(), stage, depth+1, budget)
	default:
		return nil
	}
}

// FindDiagnostic reads the first explicit application annotation in cause order.
// It is used for action selection; it does not infer provider-specific stages.
func FindDiagnostic(cause error) (FailureDiagnostic, bool) {
	budget := maximumFailureNodes
	return findDiagnostic(cause, 0, &budget)
}
func findDiagnostic(cause error, depth int, budget *int) (FailureDiagnostic, bool) {
	if cause == nil || depth >= maximumFailureDepth || *budget == 0 {
		return FailureDiagnostic{}, false
	}
	*budget--
	if diagnostic, ok := DiagnosticFor(cause); ok {
		return diagnostic, true
	}
	//nolint:errorlint
	switch value := cause.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range value.Unwrap() {
			if diagnostic, ok := findDiagnostic(child, depth+1, budget); ok {
				return diagnostic, true
			}
		}
	case interface{ Unwrap() error }:
		return findDiagnostic(value.Unwrap(), depth+1, budget)
	}
	return FailureDiagnostic{}, false
}
