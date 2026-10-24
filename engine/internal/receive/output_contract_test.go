package receive

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
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
