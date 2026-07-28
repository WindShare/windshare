//go:build linux

package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

func TestLinuxOutputAncestryTraceSeparatesAuthorityFromIdentityContradictions(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	for _, test := range []struct {
		name  string
		cause error
		want  FilesystemOutputAncestryDecision
	}{
		{
			name:  "authority denied",
			cause: outputfault.ErrAncestryAuthorityDenied,
			want:  FilesystemOutputAncestryAuthorityDenied,
		},
		{
			name:  "identity contradiction",
			cause: outputcap.ErrUnsafeNamespace,
			want:  FilesystemOutputAncestryStructuralUnsafe,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			certifyLinuxExt4AuthorityTestRoot(t, rootPath)
			var events []FilesystemOutputTrace
			authority := newLinuxNativeDecoratedPublicAuthority(
				t,
				rootPath,
				FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
					events = append(events, event)
				}),
				func(platform outputcap.Platform) outputcap.Platform {
					return &linuxAncestryTracePlatform{Platform: platform, guardErr: test.cause}
				},
			)
			session, err := authority.OpenSelection(context.Background(), linuxNativeRootFileSelection(t, 1))
			if session != nil {
				_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
				t.Fatal("ancestry failure opened an output session")
			}
			if !errors.Is(err, test.cause) {
				t.Fatalf("admission failure = %v, want wrapped %v", err, test.cause)
			}
			for _, event := range events {
				if event.Operation != TraceAncestryValidation {
					continue
				}
				if event.AncestryBoundary != FilesystemOutputAncestryAdmission ||
					event.AncestryDecision != test.want || !event.Failed {
					t.Fatalf("public ancestry trace = %+v, want decision=%v", event, test.want)
				}
				return
			}
			t.Fatalf("public ancestry trace missing from %+v", events)
		})
	}
}

// linuxAncestryTracePlatform injects admission evidence before the runtime can
// create state, which proves the trace projection preserves the distinct safe
// response for authority loss versus a structural contradiction.
type linuxAncestryTracePlatform struct {
	outputcap.Platform
	guardErr error
}

func (platform *linuxAncestryTracePlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	return nil, platform.guardErr
}
