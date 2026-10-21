package commandprojection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
)

func TestSourceFailuresPreserveMeaningAndSelectedPathThroughCleanup(t *testing.T) {
	const selected = "selected/文件.bin"
	const providerText = "opaque provider text"
	for _, test := range []struct {
		reason catalog.SourceFailure
		code   clievent.FailureCode
	}{
		{catalog.SourceFailureMissing, clievent.FailureSourceMissing},
		{catalog.SourceFailureAccessDenied, clievent.FailureSourceAccessDenied},
		{catalog.SourceFailureUnsupported, clievent.FailureSourceUnsupported},
		{catalog.SourceFailureStale, clievent.FailureSourceRevisionChanged},
		{catalog.SourceFailureUnavailable, clievent.FailureSourceUnavailable},
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
				&osfs.SelectedPathError{Path: selected, Cause: source}, context.DeadlineExceeded,
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
	cycle := &osfs.SelectedPathError{Path: "selected"}
	cycle.Cause = cycle
	for _, cause := range []error{
		cycle,
		(*osfs.SelectedPathError)(nil),
		(*catalog.SourceError)(nil),
		&catalog.SourceError{Failure: catalog.SourceFailure(255)},
	} {
		failure, present := ClassifyError(cause)
		if !present || failure.Code() != clievent.FailureUnexpected {
			t.Fatalf("invalid source failure %T = %+v, %v", cause, failure, present)
		}
	}
	failure, _ := ClassifyError(&osfs.SelectedPathError{Path: "selected", Cause: context.Canceled})
	if failure.Code() != clievent.FailureCanceled {
		t.Fatalf("cancellation became a source failure: %+v", failure)
	}
}
