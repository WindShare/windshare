package filesystem

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/osfs"
)

type testFilesystemDiagnostic struct {
	value osfs.FilesystemOutputDiagnostic
}

func (d *testFilesystemDiagnostic) Error() string { return "native failure" }
func (d *testFilesystemDiagnostic) FilesystemOutputDiagnostic() osfs.FilesystemOutputDiagnostic {
	return d.value
}

type failingNativeAuthority struct {
	nativeFilesystemOutputAuthority
	cause error
}

func (a failingNativeAuthority) BindDestination(context.Context) (osfs.FilesystemOutputExecutionMode, error) {
	return osfs.FilesystemOutputExecutionMode{}, a.cause
}
func (a failingNativeAuthority) SelectOperation(context.Context, transfer.SelectionSpec, receivecontract.OperationSelection) (osfs.FilesystemOutputLookup, error) {
	return osfs.FilesystemOutputLookup{}, a.cause
}
func (a failingNativeAuthority) CreateOperation(context.Context, osfs.FilesystemOutputLookup, receivecontract.ArtifactSpec) (osfs.FilesystemOutputOperation, error) {
	return osfs.FilesystemOutputOperation{}, a.cause
}
func (a failingNativeAuthority) Close() error { return a.cause }

func TestNativeFailureProjectionSeparatesCleanupRegardlessOfCauseOrder(t *testing.T) {
	primary := &testFilesystemDiagnostic{value: osfs.FilesystemOutputDiagnostic{Stage: osfs.FilesystemOutputFailureCheckpointReconciliation}}
	closeA := &testFilesystemDiagnostic{value: osfs.FilesystemOutputDiagnostic{Stage: osfs.FilesystemOutputFailureAuthorityClose}}
	closeB := &testFilesystemDiagnostic{value: osfs.FilesystemOutputDiagnostic{Stage: osfs.FilesystemOutputFailureAuthorityClose}}
	for _, causes := range [][]error{{primary, closeA, closeB}, {closeA, primary, closeB}} {
		original := fmt.Errorf("native operation: %w", errors.Join(causes...))
		projected := sealFilesystemOutputFailure(original)
		if !errors.Is(projected, original) || projected.Error() != original.Error() {
			t.Fatalf("native error context changed: %v", projected)
		}
		cleanup := output.FailureForStage(projected, output.FailureClose)
		if !errors.Is(cleanup, closeA) || !errors.Is(cleanup, closeB) || errors.Is(cleanup, primary) {
			t.Fatalf("cleanup claimed unrelated work: %v", cleanup)
		}
	}
}

func TestFilesystemAdapterSealsNativeDiagnosticWithoutReplacingCause(t *testing.T) {
	diagnostic := osfs.FilesystemOutputDiagnostic{Stage: osfs.FilesystemOutputFailureAuthorityClose}
	cause := &testFilesystemDiagnostic{value: diagnostic}
	native := failingNativeAuthority{cause: cause}
	authority := &filesystemOutputAuthority{native: native}
	operations := []func() error{
		func() error { _, err := authority.BindDestination(context.Background()); return err },
		func() error {
			_, err := authority.SelectOperation(context.Background(), transfer.SelectionSpec{}, receivecontract.AutomaticOperation())
			return err
		},
		func() error {
			_, err := (filesystemReservation{authority: native}).Create(context.Background(), receivecontract.ArtifactSpec{})
			return err
		},
		authority.Close,
	}
	for _, operation := range operations {
		err := operation()
		got, ok := output.DiagnosticFor(err)
		if !ok || got.Stage != output.FailureClose || !errors.Is(err, cause) || err.Error() != cause.Error() {
			t.Fatalf("diagnostic=%+v present=%v error=%v", got, ok, err)
		}
		if _, ok := output.DiagnosticFor(fmt.Errorf("arbitrary wrapper: %w", err)); ok {
			t.Fatal("direct proof traversed untrusted wrapper")
		}
	}
	if _, ok := output.DiagnosticFor(cause); ok {
		t.Fatal("arbitrary provider forged native diagnostic")
	}
	if _, ok := output.DiagnosticFor((error)(nil)); ok {
		t.Fatal("nil diagnostic carrier accepted")
	}
	unknown := errors.New("opaque provider failure")
	if sealFilesystemOutputFailure(unknown) != unknown || sealFilesystemOutputFailure(nil) != nil {
		t.Fatal("unclassified errors changed identity")
	}
}
