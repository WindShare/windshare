package liveshare

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

type catalogStorageSecretError struct {
	secret string
	called bool
}

func (failure *catalogStorageSecretError) Error() string {
	failure.called = true
	return failure.secret
}

func TestCatalogStorageCauseIsClosedAndNeverRetainsProviderText(t *testing.T) {
	secret := "credential-bearing-provider-url?token=canary"
	budgetProvider := &catalogStorageSecretError{secret: secret}
	unexpectedProvider := &catalogStorageSecretError{secret: secret}
	tests := []struct {
		name string
		err  error
		want CatalogStorageCause
	}{
		{name: "none", want: CatalogStorageCauseNone},
		{name: "canceled", err: fmt.Errorf("wrapped: %w", context.Canceled), want: CatalogStorageCauseCanceled},
		{name: "deadline", err: context.DeadlineExceeded, want: CatalogStorageCauseDeadlineExceeded},
		{name: "budget", err: errors.Join(budgetProvider, catalog.ErrBudgetExceeded), want: CatalogStorageCauseBudgetExceeded},
		{name: "unexpected", err: unexpectedProvider, want: CatalogStorageCauseUnexpected},
		{name: "cyclic", err: newRootPrefetchFailureCycle(), want: CatalogStorageCauseUnexpected},
		{name: "panicking unwrap", err: panicUnwrapError{}, want: CatalogStorageCauseUnexpected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := catalogStorageCause(test.err)
			if got != test.want {
				t.Fatalf("catalog storage cause = %v, want %v", got, test.want)
			}
			event := CatalogStorageTrace{Operation: CatalogStorageRecovered, Cause: got}
			if strings.Contains(fmt.Sprintf("%+v", event), secret) {
				t.Fatal("catalog storage trace retained provider error text")
			}
		})
	}
	if budgetProvider.called || unexpectedProvider.called {
		t.Fatal("catalog storage classification requested provider error text")
	}
}

func TestCatalogStorageCauseNamesAreClosed(t *testing.T) {
	tests := []struct {
		cause CatalogStorageCause
		want  string
	}{
		{CatalogStorageCauseNone, "none"},
		{CatalogStorageCauseCanceled, "canceled"},
		{CatalogStorageCauseDeadlineExceeded, "deadline-exceeded"},
		{CatalogStorageCauseBudgetExceeded, "budget-exceeded"},
		{CatalogStorageCauseUnexpected, "unexpected"},
		{CatalogStorageCause(255), "unknown"},
	}
	for _, test := range tests {
		if got := test.cause.String(); got != test.want {
			t.Fatalf("catalog storage cause %d = %q, want %q", test.cause, got, test.want)
		}
	}
}

func TestTraceCatalogStorageIsOptionalAndPanicIsolated(t *testing.T) {
	traceCatalogStorage(nil, CatalogStorageTrace{})
	traceCatalogStorage(
		CatalogStorageTraceFunc(func(CatalogStorageTrace) { panic("diagnostic failure") }),
		CatalogStorageTrace{},
	)

	called := false
	traceCatalogStorage(CatalogStorageTraceFunc(func(CatalogStorageTrace) { called = true }), CatalogStorageTrace{})
	if !called {
		t.Fatal("explicit catalog storage tracer was not called")
	}
}
