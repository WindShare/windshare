package receive

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestPortableOutputDestinationIsOnlyAnOpaqueDisplayLabel(t *testing.T) {
	selection := getReopenSelection(t, true, nil)
	decision, err := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	authority := &layoutRecordingGetOutputAuthority{selection: selection, events: &events}
	admitted, err := resolveGetOutputOperation(context.Background(), authority, &fixedGetShapeResolver{decision: decision}, selection)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"", "Photos / Shared album", "content://app/document/17"} {
		got, adjusted, err := getOperationDestination(label, admitted.operation)
		if err != nil || got != label || adjusted {
			t.Fatalf("label=%q got=%q adjusted=%v err=%v", label, got, adjusted, err)
		}
	}
	admitted.operation.Destination = "Shared album / received"
	admitted.operation.DestinationAdjusted = true
	got, adjusted, err := getOperationDestination("caller hint", admitted.operation)
	if err != nil || got != admitted.operation.Destination || !adjusted {
		t.Fatalf("destination=%q adjusted=%v err=%v", got, adjusted, err)
	}
	if _, _, err := getOperationDestination("valid label", OutputOperation{}); !errors.Is(err, errGetOutputReservationContract) {
		t.Fatalf("invalid operation=%v", err)
	}
}

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
func (a failingNativeAuthority) LookupActive(context.Context, transfer.SelectionSpec) (osfs.FilesystemOutputLookup, error) {
	return osfs.FilesystemOutputLookup{}, a.cause
}
func (a failingNativeAuthority) CreateOperation(context.Context, osfs.FilesystemOutputLookup, receivecontract.ArtifactSpec) (osfs.FilesystemOutputOperation, error) {
	return osfs.FilesystemOutputOperation{}, a.cause
}
func (a failingNativeAuthority) Close() error { return a.cause }

func TestFilesystemAdapterSealsNativeDiagnosticWithoutReplacingCause(t *testing.T) {
	diagnostic := osfs.FilesystemOutputDiagnostic{Stage: osfs.FilesystemOutputFailureAuthorityClose}
	cause := &testFilesystemDiagnostic{value: diagnostic}
	native := failingNativeAuthority{cause: cause}
	authority := &filesystemOutputAuthority{native: native}
	operations := []func() error{
		func() error { _, err := authority.BindDestination(context.Background()); return err },
		func() error {
			_, err := authority.LookupActive(context.Background(), transfer.SelectionSpec{})
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
		got, ok := OutputDiagnostic(err)
		if !ok || got != diagnostic || !errors.Is(err, cause) || err.Error() != cause.Error() {
			t.Fatalf("diagnostic=%+v present=%v error=%v", got, ok, err)
		}
		if _, ok := OutputDiagnostic(fmt.Errorf("arbitrary wrapper: %w", err)); ok {
			t.Fatal("direct proof traversed untrusted wrapper")
		}
	}
	if _, ok := OutputDiagnostic(cause); ok {
		t.Fatal("arbitrary provider forged native diagnostic")
	}
	if _, ok := OutputDiagnostic((*filesystemFailure)(nil)); ok {
		t.Fatal("nil diagnostic carrier accepted")
	}
	unknown := errors.New("opaque provider failure")
	if sealFilesystemOutputFailure(unknown) != unknown || sealFilesystemOutputFailure(nil) != nil {
		t.Fatal("unclassified errors changed identity")
	}
}

func TestOutputAdmissionFailureClassifiesOwnedStatesWithoutNetworkSideEffects(t *testing.T) {
	for _, cause := range []error{errGetOutputOperationAlreadyRunning, errGetOutputOperationNeedsAttention, errGetOutputOperationAmbiguous, errGetOutputReservationContract, context.Canceled} {
		observation, _ := newTestObservation(t)
		code := reportGetOutputAdmissionFailure(observation, cause)
		failure := observation.failureSnapshot()
		if code == stepReady || (failure.Cause == nil && failure.Code == 0) {
			t.Fatalf("cause=%v code=%v failure=%+v", cause, code, failure)
		}
	}
}
