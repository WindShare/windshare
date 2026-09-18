package receive

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var errGetOutputAdapterContract = errors.New("receive output authority violated its contract")

type OutputMode uint8

const (
	OutputResumable OutputMode = iota + 1
	OutputLiveOnly
)

func (mode OutputMode) valid() bool { return mode == OutputResumable || mode == OutputLiveOnly }

type OutputLookupKind uint8

const (
	OutputLookupMiss OutputLookupKind = iota + 1
	OutputLookupReopened
	OutputLookupAlreadyRunning
	OutputLookupNeedsAttention
	OutputLookupAmbiguous
)

// OutputOperation couples immutable intent with the authority that can open it.
// It does not expose a filesystem path or a provider-specific operation handle.
type OutputOperation struct {
	Intent       transfer.ReceiveIntent
	Mode         OutputMode
	Materializer transfer.DirectTreeMaterializer
	// Destination is an optional provider-owned display label, never output authority.
	Destination         string
	DestinationAdjusted bool
}

func (o OutputOperation) valid() bool {
	return !o.Intent.IsZero() && o.Mode.valid() && o.Materializer != nil
}

type OutputReservation interface {
	Create(context.Context, receivecontract.ArtifactSpec) (OutputOperation, error)
}
type OutputLookup struct {
	Kind        OutputLookupKind
	Operation   OutputOperation
	Reservation OutputReservation
}

func (lookup OutputLookup) valid() bool {
	switch lookup.Kind {
	case OutputLookupMiss:
		return !lookup.Operation.valid() && lookup.Reservation != nil
	case OutputLookupAlreadyRunning, OutputLookupNeedsAttention, OutputLookupAmbiguous:
		return !lookup.Operation.valid()
	case OutputLookupReopened:
		return lookup.Operation.valid()
	default:
		return false
	}
}

type OutputAuthority interface {
	BindDestination(context.Context) (OutputMode, error)
	LookupActive(context.Context, transfer.SelectionSpec) (OutputLookup, error)
	Close() error
}
type OutputConfig struct{ Tracer osfs.FilesystemOutputTracer }
type OutputFactory interface {
	NewOutputAuthority(OutputConfig) (OutputAuthority, error)
}
type OutputFactoryFunc func(OutputConfig) (OutputAuthority, error)

func (f OutputFactoryFunc) NewOutputAuthority(c OutputConfig) (OutputAuthority, error) {
	if f == nil {
		return nil, errGetOutputAdapterContract
	}
	return f(c)
}

type getOutputMode = OutputMode
type getOutputLookupKind = OutputLookupKind
type getOutputOperation = OutputOperation
type getOutputLookup = OutputLookup
type getOutputAuthority = OutputAuthority
type getOutputAuthorityConfig = OutputConfig
type getOutputAuthorityFactory = OutputFactory

const (
	getOutputResumable            = OutputResumable
	getOutputLiveOnly             = OutputLiveOnly
	getOutputLookupMiss           = OutputLookupMiss
	getOutputLookupReopened       = OutputLookupReopened
	getOutputLookupAlreadyRunning = OutputLookupAlreadyRunning
	getOutputLookupNeedsAttention = OutputLookupNeedsAttention
	getOutputLookupAmbiguous      = OutputLookupAmbiguous
)

type getOperationMaterializer struct{ operation OutputOperation }

func (m getOperationMaterializer) OpenDirectTree(ctx context.Context, intent transfer.ReceiveIntent) (transfer.DirectTreeSession, error) {
	if !m.operation.valid() || intent.IsZero() || !intent.EqualCanonical(m.operation.Intent) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return m.operation.Materializer.OpenDirectTree(ctx, intent)
}
