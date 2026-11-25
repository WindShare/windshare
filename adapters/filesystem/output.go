package filesystem

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/osfs"
)

// FilesystemOutput adapts one caller-selected container to receive authority.
// Destination binding happens after clients have initialized their
// own presentation resources, and before any remote session is established.
type FilesystemOutput struct {
	RootPath   string
	CreateRoot bool
}

func (f FilesystemOutput) NewOutputAuthority(c output.Config) (output.Authority, error) {
	native, err := osfs.NewOutputAuthority(osfs.OutputConfig{
		RootPath:    f.RootPath,
		CreateRoot:  f.CreateRoot,
		Diagnostics: c.Diagnostics,
	})
	if err != nil {
		return nil, sealOutputError(err)
	}
	absRoot, err := filepath.Abs(f.RootPath)
	if err != nil {
		return nil, sealOutputError(errors.Join(err, native.Close()))
	}
	return &outputAuthority{native: native, absRoot: absRoot}, nil
}

type outputAuthority struct {
	native  *osfs.OutputAuthority
	absRoot string
}

var _ output.Authority = (*outputAuthority)(nil)

func (authority *outputAuthority) BindDestination(ctx context.Context) error {
	return sealOutputError(authority.native.BindDestination(ctx))
}

func (authority *outputAuthority) SelectOperation(
	ctx context.Context,
	selection transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) (output.Lookup, error) {
	native, err := authority.native.SelectOperation(ctx, selection, selector)
	if err != nil {
		return output.Lookup{}, sealOutputError(err)
	}
	lookup := output.Lookup{}
	if reason := output.AttentionReason(native.Attention()); reason.Valid() {
		lookup.Attention = reason
	}
	switch native.Kind() {
	case osfs.OutputLookupCreateReady:
		lookup.Kind = output.OutputLookupCreateReady
		lookup.Reservation = outputReservation{lookup: native, absRoot: authority.absRoot}
	case osfs.OutputLookupNotFound:
		lookup.Kind = output.OutputLookupNotFound
	case osfs.OutputLookupReopened:
		lookup.Kind = output.OutputLookupReopened
		lookup.Operation, err = projectOperation(native.Operation(), authority.absRoot)
	case osfs.OutputLookupAlreadyRunning:
		lookup.Kind = output.OutputLookupAlreadyRunning
	case osfs.OutputLookupNeedsAttention:
		lookup.Kind = output.OutputLookupNeedsAttention
	case osfs.OutputLookupAmbiguous:
		lookup.Kind = output.OutputLookupAmbiguous
	default:
		err = output.ErrContract
	}
	if err != nil {
		return output.Lookup{}, sealOutputError(err)
	}
	if !lookup.Valid() {
		return output.Lookup{}, output.ErrContract
	}
	return lookup, nil
}

func (authority *outputAuthority) Close() error {
	return sealOutputError(authority.native.Close())
}

type outputReservation struct {
	lookup  osfs.OutputLookup
	absRoot string
}

func (reservation outputReservation) Create(
	ctx context.Context,
	artifact receivecontract.ArtifactSpec,
) (output.Operation, error) {
	native, err := reservation.lookup.Create(ctx, artifact)
	if err != nil {
		return output.Operation{}, sealOutputError(err)
	}
	return projectOperation(native, reservation.absRoot)
}

type outputMaterializer struct {
	operation *osfs.OutputOperation
}

func (materializer outputMaterializer) OpenDirectTree(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	session, err := materializer.operation.OpenDirectTree(ctx, intent)
	return session, sealOutputError(err)
}

type outputOperationControl struct {
	operation *osfs.OutputOperation
}

func (control outputOperationControl) Idle() bool { return control.operation.Idle() }

func (control outputOperationControl) RecordShareStopped(ctx context.Context) error {
	return sealOutputError(control.operation.RecordShareStopped(ctx))
}

func (control outputOperationControl) Discard(ctx context.Context) error {
	return sealOutputError(control.operation.Discard(ctx))
}

func (control outputOperationControl) NameAdjustments() output.NameAdjustmentSummary {
	summary := control.operation.NameAdjustments()
	if summary.Count == 0 {
		return output.NameAdjustmentSummary{}
	}
	samples := make([]output.NameAdjustment, len(summary.Samples))
	for i, sample := range summary.Samples {
		samples[i] = output.NameAdjustment{
			LogicalPath:  sample.LogicalPath,
			PhysicalPath: sample.PhysicalPath,
		}
	}
	return output.NameAdjustmentSummary{
		Count:   summary.Count,
		Samples: samples,
	}
}

var _ output.OperationControl = outputOperationControl{}

func projectOperation(native *osfs.OutputOperation, absRoot string) (output.Operation, error) {
	intent, ok := native.ReceiveIntent()
	if !ok {
		return output.Operation{}, output.ErrContract
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || reservation.IsZero() {
		return output.Operation{}, output.ErrContract
	}
	destination := filepath.Join(absRoot, reservation.PhysicalName())
	return output.Operation{
		Intent:              intent,
		Control:             outputOperationControl{operation: native},
		Materializer:        outputMaterializer{operation: native},
		Destination:         destination,
		DestinationAdjusted: reservation.CollisionIndex() > 0,
	}, nil
}
