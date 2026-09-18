package receive

import (
	"errors"
	"fmt"
	"github.com/windshare/windshare/connectivity/relayset"
	"reflect"
)

const (
	maximumErrorTreeDepth = 64
	maximumErrorTreeNodes = 1024
)

var (
	errStructuralProbe   = errors.New("receive error identity probe")
	trustedJoinType      = reflect.TypeOf(errors.Join(errStructuralProbe, errStructuralProbe))
	trustedWrapType      = reflect.TypeOf(fmt.Errorf("%w", errStructuralProbe))
	trustedMultiWrapType = reflect.TypeOf(fmt.Errorf("%w %w", errStructuralProbe, errStructuralProbe))
)

type errorTraversal struct{ remaining int }

func containsExactError(cause, target error) (found bool) {
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	return containsExactErrorAtDepth(
		cause, target, 0, &errorTraversal{remaining: maximumErrorTreeNodes},
	)
}

func containsExactErrorAtDepth(
	cause, target error,
	depth int,
	traversal *errorTraversal,
) bool {
	if cause == nil || depth >= maximumErrorTreeDepth || traversal.remaining == 0 || typedNil(cause) {
		return false
	}
	traversal.remaining--
	if exactError(cause, target) {
		return true
	}
	switch reflect.TypeOf(cause) {
	case trustedJoinType, trustedMultiWrapType, reflect.TypeFor[*relayset.ReceiverJoinFailure]():
		children, ok := cause.(interface{ Unwrap() []error })
		if !ok {
			return false
		}
		for _, child := range children.Unwrap() {
			if containsExactErrorAtDepth(child, target, depth+1, traversal) {
				return true
			}
		}
	case trustedWrapType:
		child, ok := cause.(interface{ Unwrap() error })
		return ok && containsExactErrorAtDepth(child.Unwrap(), target, depth+1, traversal)
	}
	return false
}

func typedNil(value error) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func exactError(candidate, target error) bool {
	if candidate == nil || target == nil || reflect.TypeOf(candidate) != reflect.TypeOf(target) {
		return false
	}
	value := reflect.ValueOf(candidate)
	// Exact comparable identity is intentional. errors.Is would execute caller
	// hooks and would also broaden this predicate beyond its proof semantics.
	//nolint:errorlint
	return value.Comparable() && value.Interface() == target
}
