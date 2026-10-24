package eventproject

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestSourceFailuresPreserveMeaningAndSelectedPathThroughCleanup(t *testing.T) {
	const selected = "selected/文件.bin"
	const providerText = "opaque provider text"
	for _, test := range []struct {
		reason catalog.SourceFailure
		code   appobs.FailureCode
	}{
		{catalog.SourceFailureMissing, appobs.FailureSourceMissing},
		{catalog.SourceFailureAccessDenied, appobs.FailureSourceAccessDenied},
		{catalog.SourceFailureUnsupported, appobs.FailureSourceUnsupported},
		{catalog.SourceFailureStale, appobs.FailureSourceRevisionChanged},
		{catalog.SourceFailureUnavailable, appobs.FailureSourceUnavailable},
	} {
		name, _ := test.code.Name()
		t.Run(name, func(t *testing.T) {
			source := &catalog.SourceError{Failure: test.reason, Cause: opaqueCanaryError{providerText}}
			plain, present := ClassifyError(source)
			if !present || plain.Code() != test.code {
				t.Fatalf("source failure = %+v, %v", plain, present)
			}
			if _, present := plain.SourcePath(); present {
				t.Fatal("invented a path for a transport-neutral file source")
			}
			cause := fmt.Errorf("prepare: %w", errors.Join(
				&catalog.SelectionError{Label: selected, Cause: source}, context.DeadlineExceeded,
			))
			failure, present := ClassifyError(cause)
			path, located := failure.SourcePath()
			if !present || !failure.Valid() || failure.Code() != test.code || !located || path.Text() != selected {
				t.Fatalf("selected source failure = %+v, %v", failure, present)
			}
			if strings.Contains(fmt.Sprintf("%+v", failure), providerText) {
				t.Fatal("provider text escaped into product classification")
			}
		})
	}
}

func TestSourceFailureProjectionKeepsTraversalBounded(t *testing.T) {
	cycle := &catalog.SelectionError{Label: "selected"}
	cycle.Cause = cycle
	for _, cause := range []error{
		cycle,
		(*catalog.SelectionError)(nil),
		(*catalog.SourceError)(nil),
		&catalog.SourceError{Failure: catalog.SourceFailure(255)},
	} {
		failure, present := ClassifyError(cause)
		if !present || failure.Code() != appobs.FailureUnexpected {
			t.Fatalf("invalid source failure %T = %+v, %v", cause, failure, present)
		}
	}
	failure, _ := ClassifyError(&catalog.SelectionError{Label: "selected", Cause: context.Canceled})
	if failure.Code() != appobs.FailureCanceled {
		t.Fatalf("cancellation became a source failure: %+v", failure)
	}
}
