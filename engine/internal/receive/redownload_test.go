//go:build windows || linux || darwin

package receive

import (
	"context"
	"testing"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestRedownloadPreservesAutomaticContinuationAndExactTaskSelection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	selection := getReopenSelection(t, true, nil)
	shape, _ := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
	admit := func(policy OutputAdmission, id receivecontract.OperationID) getOutputOperation {
		t.Helper()
		a, err := (filesystem.FilesystemOutput{RootPath: root}).NewOutputAuthority(OutputConfig{})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if _, err := a.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		admitted, err := resolveGetOutputOperation(ctx, a, &fixedGetShapeResolver{decision: shape}, selection, policy, id)
		if err != nil {
			t.Fatal(err)
		}
		return admitted.operation
	}
	original := admit(OutputCreateOrResume, receivecontract.OperationID{})
	fresh := admit(OutputRedownload, receivecontract.OperationID{})
	if fresh.Intent.OperationID() == original.Intent.OperationID() || fresh.Destination == original.Destination || !fresh.DestinationAdjusted {
		t.Fatal("redownload inherited the original identity or destination")
	}
	if automatic := admit(OutputCreateOrResume, receivecontract.OperationID{}); !automatic.Intent.EqualCanonical(original.Intent) {
		t.Fatal("redownload replaced automatic continuation")
	}
	wrong, err := (filesystem.FilesystemOutput{RootPath: root}).NewOutputAuthority(OutputConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	wrongSelection := getReopenSelection(t, false, []string{"unrelated"})
	if _, err := resolveGetOutputOperation(ctx, wrong, &fixedGetShapeResolver{decision: shape}, wrongSelection, OutputResumeExisting, original.Intent.OperationID()); err == nil {
		t.Fatal("operation ID accepted a different selection")
	}
	if err := wrong.Close(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []getOutputOperation{original, fresh} {
		if retried := admit(OutputResumeExisting, expected.Intent.OperationID()); !retried.Intent.EqualCanonical(expected.Intent) {
			t.Fatal("retry selected the wrong operation")
		}
	}
}
