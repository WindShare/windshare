package cli

import (
	"context"
	"errors"

	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var errGetOutputAdapterContract = errors.New("get output authority adapter violated its contract")

type getOutputMode uint8

const (
	getOutputResumable getOutputMode = iota + 1
	getOutputLiveOnly
)

func (mode getOutputMode) valid() bool {
	return mode == getOutputResumable || mode == getOutputLiveOnly
}

type getOutputLookupKind uint8

const (
	getOutputLookupMiss getOutputLookupKind = iota + 1
	getOutputLookupReopened
	getOutputLookupAlreadyRunning
	getOutputLookupNeedsAttention
	getOutputLookupAmbiguous
)

type getOutputOperation struct {
	intent transfer.ReceiveIntent
	mode   getOutputMode
	native osfs.FilesystemOutputOperation
}

func (operation getOutputOperation) valid() bool {
	return !operation.intent.IsZero() && operation.mode.valid()
}

type getOutputLookup struct {
	kind      getOutputLookupKind
	operation getOutputOperation
	native    osfs.FilesystemOutputLookup
}

func (lookup getOutputLookup) valid() bool {
	switch lookup.kind {
	case getOutputLookupMiss, getOutputLookupAlreadyRunning,
		getOutputLookupNeedsAttention, getOutputLookupAmbiguous:
		return !lookup.operation.valid()
	case getOutputLookupReopened:
		return lookup.operation.valid()
	default:
		return false
	}
}

type getOutputAuthority interface {
	BindDestination(context.Context) (getOutputMode, error)
	LookupActive(context.Context, transfer.SelectionSpec) (getOutputLookup, error)
	CreateOperation(
		context.Context,
		getOutputLookup,
		receivecontract.ArtifactSpec,
	) (getOutputOperation, error)
	OpenOperation(context.Context, getOutputOperation) (transfer.DirectTreeSession, error)
	Close() error
}

type getOutputAuthorityConfig struct {
	rootPath   string
	createRoot bool
	tracer     osfs.FilesystemOutputTracer
}

type getOutputAuthorityFactory interface {
	NewGetOutputAuthority(getOutputAuthorityConfig) (getOutputAuthority, error)
}

type getOutputAuthorityFactoryFunc func(getOutputAuthorityConfig) (getOutputAuthority, error)

func (function getOutputAuthorityFactoryFunc) NewGetOutputAuthority(
	config getOutputAuthorityConfig,
) (getOutputAuthority, error) {
	if function == nil {
		return nil, errGetOutputAdapterContract
	}
	return function(config)
}

type filesystemGetOutputAuthority struct {
	native nativeFilesystemOutputAuthority
}

type nativeFilesystemOutputAuthority interface {
	BindDestination(context.Context) (osfs.FilesystemOutputExecutionMode, error)
	LookupActive(context.Context, transfer.SelectionSpec) (osfs.FilesystemOutputLookup, error)
	CreateOperation(
		context.Context,
		osfs.FilesystemOutputLookup,
		receivecontract.ArtifactSpec,
	) (osfs.FilesystemOutputOperation, error)
	OpenOperation(
		context.Context,
		osfs.FilesystemOutputOperation,
	) (transfer.DirectTreeSession, error)
	Close() error
}

func newFilesystemGetOutputAuthority(config getOutputAuthorityConfig) (getOutputAuthority, error) {
	native, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: config.rootPath, CreateRoot: config.createRoot, Tracer: config.tracer,
	})
	if err != nil {
		return nil, sealFilesystemOutputFailure(err)
	}
	return &filesystemGetOutputAuthority{native: native}, nil
}

func (authority *filesystemGetOutputAuthority) BindDestination(
	ctx context.Context,
) (getOutputMode, error) {
	if authority == nil || authority.native == nil {
		return 0, errGetOutputAdapterContract
	}
	mode, err := authority.native.BindDestination(ctx)
	if err != nil {
		return 0, sealFilesystemOutputFailure(err)
	}
	switch {
	case mode.Resumable():
		return getOutputResumable, nil
	case mode.LiveOnly():
		return getOutputLiveOnly, nil
	default:
		return 0, errGetOutputAdapterContract
	}
}

func (authority *filesystemGetOutputAuthority) LookupActive(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (getOutputLookup, error) {
	if authority == nil || authority.native == nil {
		return getOutputLookup{}, errGetOutputAdapterContract
	}
	lookup, err := authority.native.LookupActive(ctx, selection)
	if err != nil {
		return getOutputLookup{}, sealFilesystemOutputFailure(err)
	}
	converted := getOutputLookup{native: lookup}
	switch lookup.Kind() {
	case osfs.FilesystemOutputLookupMiss:
		converted.kind = getOutputLookupMiss
	case osfs.FilesystemOutputLookupReopened:
		converted.kind = getOutputLookupReopened
		converted.operation, err = getFilesystemOutputOperation(lookup.Operation())
	case osfs.FilesystemOutputLookupAlreadyRunning:
		converted.kind = getOutputLookupAlreadyRunning
	case osfs.FilesystemOutputLookupNeedsAttention:
		converted.kind = getOutputLookupNeedsAttention
	case osfs.FilesystemOutputLookupAmbiguous:
		converted.kind = getOutputLookupAmbiguous
	default:
		err = errGetOutputAdapterContract
	}
	if err != nil || !converted.valid() {
		return getOutputLookup{}, errors.Join(errGetOutputAdapterContract, err)
	}
	return converted, nil
}

func (authority *filesystemGetOutputAuthority) CreateOperation(
	ctx context.Context,
	lookup getOutputLookup,
	artifact receivecontract.ArtifactSpec,
) (getOutputOperation, error) {
	if authority == nil || authority.native == nil || lookup.kind != getOutputLookupMiss ||
		!lookup.valid() || artifact.IsZero() {
		return getOutputOperation{}, errGetOutputAdapterContract
	}
	operation, err := authority.native.CreateOperation(ctx, lookup.native, artifact)
	if err != nil {
		return getOutputOperation{}, sealFilesystemOutputFailure(err)
	}
	return getFilesystemOutputOperation(operation)
}

func (authority *filesystemGetOutputAuthority) OpenOperation(
	ctx context.Context,
	operation getOutputOperation,
) (transfer.DirectTreeSession, error) {
	if authority == nil || authority.native == nil || !operation.valid() {
		return nil, errGetOutputAdapterContract
	}
	session, err := authority.native.OpenOperation(ctx, operation.native)
	return session, sealFilesystemOutputFailure(err)
}

func (authority *filesystemGetOutputAuthority) Close() error {
	if authority == nil || authority.native == nil {
		return nil
	}
	return sealFilesystemOutputFailure(authority.native.Close())
}

func sealFilesystemOutputFailure(cause error) error {
	if cause == nil {
		return nil
	}
	diagnostic, ok := osfs.FilesystemOutputDiagnosticFor(cause)
	if !ok {
		return cause
	}
	sealed, ok := commandprojection.SealFilesystemOutputFailure(diagnostic)
	if !ok {
		return errGetOutputAdapterContract
	}
	return sealed
}

func getFilesystemOutputOperation(
	native osfs.FilesystemOutputOperation,
) (getOutputOperation, error) {
	intent, ok := native.ReceiveIntent()
	if !ok {
		return getOutputOperation{}, errGetOutputAdapterContract
	}
	mode := native.ExecutionMode()
	converted := getOutputOperation{intent: intent, native: native}
	switch {
	case mode.Resumable():
		converted.mode = getOutputResumable
	case mode.LiveOnly():
		converted.mode = getOutputLiveOnly
	default:
		return getOutputOperation{}, errGetOutputAdapterContract
	}
	if !converted.valid() {
		return getOutputOperation{}, errGetOutputAdapterContract
	}
	return converted, nil
}

type getOperationMaterializer struct {
	authority getOutputAuthority
	operation getOutputOperation
}

func (materializer getOperationMaterializer) OpenDirectTree(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	if materializer.authority == nil || !materializer.operation.valid() ||
		intent.IsZero() || !intent.EqualCanonical(materializer.operation.intent) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return materializer.authority.OpenOperation(ctx, materializer.operation)
}
