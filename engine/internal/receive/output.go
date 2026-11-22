package receive

import (
	"context"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine/output"
)

var errGetOutputAdapterContract = output.ErrContract

type OutputMode = output.Mode
type OutputLookupKind = output.LookupKind
type OutputOperation = output.Operation
type OutputReservation = output.Reservation
type OutputLookup = output.Lookup
type OutputAuthority = output.Authority
type OutputConfig = output.Config
type OutputFactoryFunc = output.FactoryFunc
type OutputFactory = output.Factory

const (
	OutputResumable            = output.OutputResumable
	OutputLiveOnly             = output.OutputLiveOnly
	OutputLookupCreateReady    = output.OutputLookupCreateReady
	OutputLookupNotFound       = output.OutputLookupNotFound
	OutputLookupReopened       = output.OutputLookupReopened
	OutputLookupAlreadyRunning = output.OutputLookupAlreadyRunning
	OutputLookupNeedsAttention = output.OutputLookupNeedsAttention
	OutputLookupAmbiguous      = output.OutputLookupAmbiguous
)

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
	getOutputLookupCreateReady    = OutputLookupCreateReady
	getOutputLookupNotFound       = OutputLookupNotFound
	getOutputLookupReopened       = OutputLookupReopened
	getOutputLookupAlreadyRunning = OutputLookupAlreadyRunning
	getOutputLookupNeedsAttention = OutputLookupNeedsAttention
	getOutputLookupAmbiguous      = OutputLookupAmbiguous
)

type getOperationMaterializer struct{ operation OutputOperation }

func (m getOperationMaterializer) OpenDirectTree(ctx context.Context, intent transfer.ReceiveIntent) (transfer.DirectTreeSession, error) {
	if !m.operation.Valid() || intent.IsZero() || !intent.EqualCanonical(m.operation.Intent) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return m.operation.Materializer.OpenDirectTree(ctx, intent)
}
