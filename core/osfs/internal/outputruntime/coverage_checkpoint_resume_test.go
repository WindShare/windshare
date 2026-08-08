package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestNativeAdapterNormalizesFailuresAtOneBoundary(t *testing.T) {
	dependency := errors.New("native adapter dependency failed")
	outputErr := runtimeOutputError(
		context.Background(),
		transferfault.OutputStateIO,
		"persist stable output",
		dependency,
	)
	if !errors.Is(outputErr, dependency) {
		t.Fatalf("output failure lost its diagnostic cause: %v", outputErr)
	}
	assertOutputRuntimeFault(t, outputErr, transferfault.OutputStateIO)

	dependencyErr := runtimeDependencyError("construct native collaborator", dependency)
	normalized := transferfault.NormalizeBoundary(context.Background(), dependencyErr)
	value, ok := normalized.Fault()
	code, session := value.SessionCode()
	if !ok || !session || code != transferfault.SessionDependencyContract ||
		value.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("dependency fault = %+v", normalized)
	}

	checkpointErr := checkpointRuntimeError(
		context.Background(), "adapt unknown checkpoint failure", dependency,
	)
	normalized = transferfault.NormalizeBoundary(context.Background(), checkpointErr)
	value, ok = normalized.Fault()
	code, session = value.SessionCode()
	if !ok || !session || code != transferfault.SessionDependencyContract ||
		value.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("unknown checkpoint fault = %+v", normalized)
	}
	if checkpointRuntimeError(context.Background(), "no failure", nil) != nil {
		t.Fatal("nil checkpoint cause became a fault")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtimeOutputError(
		canceled, transferfault.OutputStateIO, "canceled output", dependency,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled output boundary = %v", err)
	}
	if err := checkpointRuntimeError(
		canceled, "canceled checkpoint", dependency,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled checkpoint boundary = %v", err)
	}
	if err := runtimeOutputError(
		context.Background(),
		transferfault.OutputStateIO,
		"deadline output",
		context.DeadlineExceeded,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline output boundary = %v", err)
	}
	if err := checkpointRuntimeError(
		context.Background(), "deadline checkpoint", context.DeadlineExceeded,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline checkpoint boundary = %v", err)
	}
}

func TestNativeResourceReleaseIsStableAndReportsCloseFailureOnce(t *testing.T) {
	closeFailure := errors.New("platform close failed")
	platform := &releaseFailurePlatform{closeErr: closeFailure}
	resources := &nativeOutputResources{platform: platform}

	first := resources.ReleaseOutputSession(context.Background())
	second := resources.ReleaseOutputSession(context.Background())
	if !errors.Is(first, closeFailure) || !errors.Is(second, closeFailure) {
		t.Fatalf("stable release errors = %v / %v", first, second)
	}
	if platform.closeCalls != 1 {
		t.Fatalf("platform close calls = %d, want 1", platform.closeCalls)
	}
	assertOutputRuntimeFault(t, first, transferfault.OutputStateIO)
	if err := (*nativeOutputResources)(nil).ReleaseOutputSession(context.Background()); err != nil {
		t.Fatalf("nil resource release = %v", err)
	}
}

func TestNativeRootDispositionProjectionKeepsPersistedAuthorityClosed(t *testing.T) {
	tests := []struct {
		native outputcap.RootOpenDisposition
		trace  FilesystemOutputRootDisposition
	}{
		{
			native: outputcap.CallerProvidedContainer,
			trace:  FilesystemOutputCallerProvidedContainer,
		},
		{
			native: outputcap.AuthorityCreatedRoot,
			trace:  FilesystemOutputAuthorityCreatedRoot,
		},
		{native: outputcap.RootOpenDisposition("future"), trace: ""},
	}
	for _, test := range tests {
		if got := runtimeRootDisposition(test.native); got != test.trace {
			t.Fatalf("root disposition %q projected as %q, want %q", test.native, got, test.trace)
		}
	}
	wrapped := dispositionPlatform{disposition: outputcap.AuthorityCreatedRoot}
	if wrapped.RootOpenDisposition() != outputcap.AuthorityCreatedRoot {
		t.Fatal("runtime platform lost the persisted root-open disposition")
	}
}

func assertOutputRuntimeFault(
	t *testing.T,
	err error,
	want transferfault.OutputCode,
) {
	t.Helper()
	normalized := transferfault.NormalizeBoundary(context.Background(), err)
	value, ok := normalized.Fault()
	code, output := value.OutputCode()
	if !ok || !output || code != want || value.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("normalized output fault = %+v", normalized)
	}
}

type releaseFailurePlatform struct {
	outputcap.Platform
	closeErr   error
	closeCalls int
}

func (platform *releaseFailurePlatform) Close() error {
	platform.closeCalls++
	return platform.closeErr
}
