// Package failurestate admits collaborator failures into closed policy values
// shared by transfer orchestration and lane scheduling. Diagnostics cannot grant
// cancellation, retry or output-retirement authority after this boundary.
package failurestate

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/windshare/windshare/core/transfer/fault"
)

type cancellationKind uint8

const (
	cancellationNone cancellationKind = iota
	cancellationCanceled
	cancellationDeadline
)

// Failure keeps the closed policy state distinct from its raw
// diagnostic. It deliberately has no Unwrap method, so code outside the
// immediate boundary cannot reinterpret the diagnostic graph as authority.
type Failure struct {
	value        fault.Fault
	cancellation cancellationKind
	diagnostic   error
}

var failureType = reflect.TypeFor[*Failure]()

func (failure *Failure) Error() string {
	if failure == nil {
		return "transfer lifecycle failure is invalid"
	}
	switch {
	case failure.Canceled() && failure.value.Valid() && failure.diagnostic != nil:
		return fmt.Sprintf("transfer canceled with fault %s: %v", failure.value, failure.diagnostic)
	case failure.Canceled() && failure.diagnostic != nil:
		return fmt.Sprintf("transfer canceled: %v", failure.diagnostic)
	case failure.Canceled():
		return "transfer canceled"
	case failure.value.Valid() && failure.diagnostic != nil:
		return fmt.Sprintf("transfer fault %s: %v", failure.value, failure.diagnostic)
	case failure.value.Valid():
		return fmt.Sprintf("transfer fault %s", failure.value)
	default:
		return "transfer lifecycle failure is invalid"
	}
}

// Is exposes only the closed cancellation signal. Native diagnostic causes are
// intentionally not searchable after normalization.
func (failure *Failure) Is(target error) bool {
	return failure != nil && failure.Canceled() && failure.cancellation.matches(target)
}

// Admit accepts only the direct concrete carrier produced by
// this package. The dynamic-type gate prevents errors.As from granting policy
// authority to an arbitrary wrapper while still using the standard admission
// API after that exactness proof.
func Admit(err error) (*Failure, bool) {
	if err == nil || reflect.TypeOf(err) != failureType {
		return nil, false
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure == nil {
		return nil, false
	}
	return failure, true
}

func Require(err error) *Failure {
	if err == nil {
		return nil
	}
	if failure, ok := Admit(err); ok {
		return failure
	}
	return DependencyContract(err)
}

func Err(failure *Failure) error {
	if failure == nil {
		return nil
	}
	return failure
}

func (kind cancellationKind) cause() error {
	switch kind {
	case cancellationCanceled:
		return context.Canceled
	case cancellationDeadline:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (kind cancellationKind) matches(target error) bool {
	return kind != cancellationNone && errors.Is(kind.cause(), target)
}

func joinCancellation(left, right cancellationKind) cancellationKind {
	if right > left {
		return right
	}
	return left
}

var (
	contextCanceledType = reflect.TypeOf(context.Canceled)
	contextDeadlineType = reflect.TypeOf(context.DeadlineExceeded)
)

// exactCancellation admits only the standard sentinel itself. Internal joins
// must not let an arbitrary wrapper graph acquire cancellation authority after
// the collaborator boundary has already normalized it.
func exactCancellation(err error) cancellationKind {
	dynamicType := reflect.TypeOf(err)
	switch {
	case dynamicType == contextDeadlineType && errors.Is(err, context.DeadlineExceeded):
		return cancellationDeadline
	case dynamicType == contextCanceledType && errors.Is(err, context.Canceled):
		return cancellationCanceled
	default:
		return cancellationNone
	}
}

func New(value fault.Fault, diagnostic error) *Failure {
	if !value.Valid() {
		value = fault.DependencyContractFault()
	}
	return &Failure{
		value:      value,
		diagnostic: diagnostic,
	}
}

func DependencyContract(cause error) *Failure {
	return New(fault.DependencyContractFault(), cause)
}

func FaultOf(err error) fault.Fault {
	failure, ok := Admit(err)
	if !ok {
		return fault.Fault{}
	}
	return failure.value
}

func Diagnostic(err error) error {
	failure, ok := Admit(err)
	if !ok {
		return err
	}
	return failure.diagnostic
}

// Fault and cancellation remain separate because a sibling cancellation must not
// erase the fault that explains why the owning operation stopped.
func (failure *Failure) Fault() fault.Fault {
	if failure == nil {
		return fault.Fault{}
	}
	return failure.value
}
func (failure *Failure) Canceled() bool {
	return failure != nil && failure.cancellation != cancellationNone
}
func (failure *Failure) Cancellation() error {
	if failure == nil {
		return nil
	}
	return failure.cancellation.cause()
}
func (failure *Failure) Diagnostic() error {
	if failure == nil {
		return nil
	}
	return failure.diagnostic
}
