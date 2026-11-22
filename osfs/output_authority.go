package osfs

import (
	"context"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/output"
)

type OutputConfig struct {
	RootPath    string
	CreateRoot  bool
	Diagnostics diagnostics.Sink
}

// OutputAuthority owns native best-effort resumable destination storage without
// depending on application-layer engine ports.
type OutputAuthority struct {
	runtime *output.Authority
}

func NewOutputAuthority(cfg OutputConfig) (*OutputAuthority, error) {
	runtimeAuthority, err := output.New(output.Config{
		RootPath:    cfg.RootPath,
		CreateRoot:  cfg.CreateRoot,
		Diagnostics: cfg.Diagnostics,
	})
	if err != nil {
		return nil, err
	}
	return &OutputAuthority{runtime: runtimeAuthority}, nil
}

func (authority *OutputAuthority) BindDestination(ctx context.Context) error {
	if authority == nil || authority.runtime == nil {
		return output.DiagnoseFailure(output.FailureDestinationBinding, transfer.ErrInvalidOutputBinding)
	}
	return authority.runtime.BindDestination(ctx)
}

func (authority *OutputAuthority) SelectOperation(
	ctx context.Context,
	selection transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) (OutputLookup, error) {
	if authority == nil || authority.runtime == nil {
		return OutputLookup{}, output.DiagnoseFailure(output.FailureLookup, transfer.ErrInvalidOutputBinding)
	}
	native, err := authority.runtime.SelectOperation(ctx, selection, selector)
	if err != nil {
		return OutputLookup{}, err
	}
	return OutputLookup{authority: authority, native: native}, nil
}

func (authority *OutputAuthority) Close() error {
	if authority == nil || authority.runtime == nil {
		return nil
	}
	return authority.runtime.Close()
}

type OutputLookup struct {
	authority *OutputAuthority
	native    output.ActiveLookup
}

func (lookup OutputLookup) Kind() OutputLookupKind           { return lookup.native.Kind() }
func (lookup OutputLookup) Attention() OutputAttentionReason { return lookup.native.Attention() }

func (lookup OutputLookup) Operation() *OutputOperation {
	native := lookup.native.Operation()
	if lookup.authority == nil || native == nil {
		return nil
	}
	return &OutputOperation{runtime: lookup.authority.runtime, native: native}
}

func (lookup OutputLookup) Create(
	ctx context.Context,
	artifact receivecontract.ArtifactSpec,
) (*OutputOperation, error) {
	if lookup.authority == nil || lookup.authority.runtime == nil {
		return nil, output.DiagnoseFailure(output.FailureCreate, transfer.ErrInvalidOutputBinding)
	}
	native, err := lookup.authority.runtime.CreateOperation(ctx, lookup.native, artifact)
	if err != nil {
		return nil, err
	}
	return &OutputOperation{runtime: lookup.authority.runtime, native: native}, nil
}

// OutputOperation holds a frozen receive intent and its native output lock,
// directly implementing transfer.DirectTreeMaterializer.
type OutputOperation struct {
	runtime *output.Authority
	native  *output.Operation
}

var _ transfer.DirectTreeMaterializer = (*OutputOperation)(nil)

func (operation *OutputOperation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	if operation == nil || operation.native == nil {
		return transfer.ReceiveIntent{}, false
	}
	return operation.native.ReceiveIntent()
}

func (operation *OutputOperation) OpenDirectTree(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	if operation == nil || operation.runtime == nil || operation.native == nil {
		return nil, output.DiagnoseFailure(output.FailureOpen, transfer.ErrInvalidOutputBinding)
	}
	owned, ok := operation.native.ReceiveIntent()
	if !ok || !owned.EqualCanonical(intent) {
		return nil, output.DiagnoseFailure(output.FailureOpen, transfer.ErrInvalidOutputBinding)
	}
	return operation.runtime.OpenOperation(ctx, operation.native)
}

func (operation *OutputOperation) Idle() bool {
	return operation != nil && operation.native != nil && operation.native.Idle()
}

func (operation *OutputOperation) RecordShareStopped(ctx context.Context) error {
	if operation == nil || operation.native == nil {
		return transfer.ErrInvalidOutputBinding
	}
	return operation.native.RecordShareStopped(ctx)
}

func (operation *OutputOperation) Discard(ctx context.Context) error {
	if operation == nil || operation.native == nil {
		return transfer.ErrInvalidOutputBinding
	}
	return operation.native.Discard(ctx)
}
