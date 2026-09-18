package receive

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"path/filepath"
)

// FilesystemOutput adapts one caller-selected container to receive authority.
// Certification happens in the workflow, after clients have initialized their
// own presentation resources, and before any remote session is established.
type FilesystemOutput struct {
	RootPath   string
	CreateRoot bool
}

func (f FilesystemOutput) NewOutputAuthority(c OutputConfig) (OutputAuthority, error) {
	native, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{RootPath: f.RootPath, CreateRoot: f.CreateRoot, Tracer: c.Tracer})
	if err != nil {
		return nil, sealFilesystemOutputFailure(err)
	}
	rootPath, err := filepath.Abs(f.RootPath)
	if err != nil {
		return nil, errors.Join(err, native.Close())
	}
	return &filesystemOutputAuthority{native: native, rootPath: rootPath}, nil
}

type nativeFilesystemOutputAuthority interface {
	BindDestination(context.Context) (osfs.FilesystemOutputExecutionMode, error)
	LookupActive(context.Context, transfer.SelectionSpec) (osfs.FilesystemOutputLookup, error)
	CreateOperation(context.Context, osfs.FilesystemOutputLookup, receivecontract.ArtifactSpec) (osfs.FilesystemOutputOperation, error)
	OpenOperation(context.Context, osfs.FilesystemOutputOperation) (transfer.DirectTreeSession, error)
	Close() error
}
type filesystemOutputAuthority struct {
	native   nativeFilesystemOutputAuthority
	rootPath string
}

func outputMode(native osfs.FilesystemOutputExecutionMode) (OutputMode, error) {
	switch {
	case native.Resumable():
		return OutputResumable, nil
	case native.LiveOnly():
		return OutputLiveOnly, nil
	default:
		return 0, errGetOutputAdapterContract
	}
}
func (a *filesystemOutputAuthority) BindDestination(ctx context.Context) (OutputMode, error) {
	mode, err := a.native.BindDestination(ctx)
	if err != nil {
		return 0, sealFilesystemOutputFailure(err)
	}
	return outputMode(mode)
}
func (a *filesystemOutputAuthority) LookupActive(ctx context.Context, s transfer.SelectionSpec) (OutputLookup, error) {
	native, err := a.native.LookupActive(ctx, s)
	if err != nil {
		return OutputLookup{}, sealFilesystemOutputFailure(err)
	}
	lookup := OutputLookup{}
	switch native.Kind() {
	case osfs.FilesystemOutputLookupMiss:
		lookup.Kind = OutputLookupMiss
		lookup.Reservation = filesystemReservation{authority: a.native, lookup: native, rootPath: a.rootPath}
	case osfs.FilesystemOutputLookupReopened:
		lookup.Kind = OutputLookupReopened
		lookup.Operation, err = filesystemOperation(a.native, native.Operation(), a.rootPath)
	case osfs.FilesystemOutputLookupAlreadyRunning:
		lookup.Kind = OutputLookupAlreadyRunning
	case osfs.FilesystemOutputLookupNeedsAttention:
		lookup.Kind = OutputLookupNeedsAttention
	case osfs.FilesystemOutputLookupAmbiguous:
		lookup.Kind = OutputLookupAmbiguous
	default:
		err = errGetOutputAdapterContract
	}
	if err != nil {
		return OutputLookup{}, sealFilesystemOutputFailure(err)
	}
	if !lookup.valid() {
		return OutputLookup{}, errGetOutputAdapterContract
	}
	return lookup, nil
}
func (a *filesystemOutputAuthority) Close() error {
	return sealFilesystemOutputFailure(a.native.Close())
}

type filesystemReservation struct {
	authority nativeFilesystemOutputAuthority
	lookup    osfs.FilesystemOutputLookup
	rootPath  string
}

func (r filesystemReservation) Create(ctx context.Context, artifact receivecontract.ArtifactSpec) (OutputOperation, error) {
	native, err := r.authority.CreateOperation(ctx, r.lookup, artifact)
	if err != nil {
		return OutputOperation{}, sealFilesystemOutputFailure(err)
	}
	return filesystemOperation(r.authority, native, r.rootPath)
}

type filesystemMaterializer struct {
	authority nativeFilesystemOutputAuthority
	operation osfs.FilesystemOutputOperation
}

func (m filesystemMaterializer) OpenDirectTree(ctx context.Context, intent transfer.ReceiveIntent) (transfer.DirectTreeSession, error) {
	owned, ok := m.operation.ReceiveIntent()
	if !ok || !owned.EqualCanonical(intent) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	session, err := m.authority.OpenOperation(ctx, m.operation)
	return session, sealFilesystemOutputFailure(err)
}
func filesystemOperation(authority nativeFilesystemOutputAuthority, native osfs.FilesystemOutputOperation, rootPath string) (OutputOperation, error) {
	intent, ok := native.ReceiveIntent()
	if !ok {
		return OutputOperation{}, errGetOutputAdapterContract
	}
	mode, err := outputMode(native.ExecutionMode())
	if err != nil {
		return OutputOperation{}, sealFilesystemOutputFailure(err)
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || reservation.IsZero() {
		return OutputOperation{}, errGetOutputAdapterContract
	}
	destination := rootPath
	if reservation.Kind() == receivecontract.ReservationNamedContainerEntry {
		destination = filepath.Join(rootPath, reservation.PhysicalName())
	}
	return OutputOperation{Intent: intent, Mode: mode, Materializer: filesystemMaterializer{authority: authority, operation: native}, Destination: destination, DestinationAdjusted: reservation.CollisionIndex() > 0}, nil
}
