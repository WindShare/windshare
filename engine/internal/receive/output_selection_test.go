//go:build windows || linux || darwin

package receive

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestResumeMissingOperationReportsDomainFailureBeforeShapeResolution(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "automatic"
		if explicit {
			name = "selected"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			authority, err := (filesystem.FilesystemOutput{
				RootPath: newCLICertifiedOutputTestRoot(t), CreateRoot: true,
			}).NewOutputAuthority(OutputConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer authority.Close()
			if mode, err := authority.BindDestination(ctx); err != nil || mode != OutputResumable {
				t.Fatalf("bind = %v, %v", mode, err)
			}
			var id receivecontract.OperationID
			if explicit {
				id[0] = 1
			}
			resolver := &fixedGetShapeResolver{err: errors.New("missing resume must not resolve a new download")}
			admission, err := resolveGetOutputOperation(
				ctx, authority, resolver, getReopenSelection(t, true, nil), OutputResumeExisting, id,
			)
			if !errors.Is(err, ErrRetainedOutputMissing) || admission.operation.Valid() || resolver.calls != 0 {
				t.Fatalf("missing resume = %+v, %v; shape calls = %d", admission, err, resolver.calls)
			}
			observation := getObservation{observationState: &observationState{}}
			if result := reportGetOutputAdmissionFailure(observation, err); result != stepLocalFailure ||
				observation.failureCode != FailureRetainedOutputMissing {
				t.Fatalf("missing resume lost its failure code: %v, %v", result, observation.failureCode)
			}
		})
	}
}

func TestAutomaticSelectionRejectsExplicitNotFoundResult(t *testing.T) {
	resolver := &fixedGetShapeResolver{}
	_, err := resolveGetOutputOperation(context.Background(),
		fixedLookupGetOutputAuthority{lookup: getOutputLookup{Kind: getOutputLookupNotFound}},
		resolver, getReopenSelection(t, true, nil), OutputCreateOrResume, receivecontract.OperationID{})
	if !errors.Is(err, errGetOutputReservationContract) || resolver.calls != 0 {
		t.Fatalf("provider's contradictory result = %v; shape calls = %d", err, resolver.calls)
	}
}
