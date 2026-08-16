package liveshare

import (
	"context"
	"reflect"

	"github.com/windshare/windshare/core/catalog"
)

const maximumCatalogStorageFailureNodes = 64

// catalogStorageCause inspects only trusted sentinel identity. It never asks a
// provider error for text, and the work bound prevents a cyclic collaborator
// graph from stalling catalog lifecycle reporting.
//
//nolint:errorlint // Recursive errors.Is cannot provide a graph work bound.
func catalogStorageCause(root error) (cause CatalogStorageCause) {
	if root == nil {
		return CatalogStorageCauseNone
	}
	cause = CatalogStorageCauseUnexpected
	defer func() {
		if recover() != nil {
			cause = CatalogStorageCauseUnexpected
		}
	}()

	var canceled, deadline, budget bool
	pending := []error{root}
	for inspected := 0; len(pending) != 0 && inspected < maximumCatalogStorageFailureNodes; inspected++ {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current == nil {
			continue
		}
		if reflect.TypeOf(current).Comparable() {
			switch current {
			case catalog.ErrBudgetExceeded:
				budget = true
			case context.Canceled:
				canceled = true
			case context.DeadlineExceeded:
				deadline = true
			}
		}
		remaining := maximumCatalogStorageFailureNodes - inspected - 1 - len(pending)
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if remaining > 0 {
				pending = append(pending, children[:min(len(children), remaining)]...)
			}
		case interface{ Unwrap() error }:
			if remaining > 0 {
				pending = append(pending, wrapped.Unwrap())
			}
		}
	}
	switch {
	case budget:
		return CatalogStorageCauseBudgetExceeded
	case canceled:
		return CatalogStorageCauseCanceled
	case deadline:
		return CatalogStorageCauseDeadlineExceeded
	default:
		return cause
	}
}
