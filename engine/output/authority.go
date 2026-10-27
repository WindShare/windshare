// Package output owns destination acquisition and receive output contracts.
package output

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrContract = errors.New("receive output authority violated its contract")

type Mode uint8

const (
	OutputResumable Mode = iota + 1
	OutputLiveOnly
)

func (mode Mode) Valid() bool { return mode == OutputResumable || mode == OutputLiveOnly }

type LookupKind uint8

const (
	OutputLookupMiss LookupKind = iota + 1
	OutputLookupReopened
	OutputLookupAlreadyRunning
	OutputLookupNeedsAttention
	OutputLookupAmbiguous
)

// Operation couples immutable intent with the authority that can open it.
// It does not expose a filesystem path or a provider-specific operation handle.
type OperationControl interface {
	RetryAvailable() bool
	Discard(context.Context) error
}

type Operation struct {
	Intent       transfer.ReceiveIntent
	Mode         Mode
	Materializer transfer.DirectTreeMaterializer
	Control      OperationControl
	// Destination is an optional provider-owned display label, never output authority.
	Destination         string
	DestinationAdjusted bool
}

func (o Operation) Valid() bool {
	return !o.Intent.IsZero() && o.Mode.Valid() && o.Materializer != nil
}

type Reservation interface {
	Create(context.Context, receivecontract.ArtifactSpec) (Operation, error)
}

// AttentionError preserves an admission reason through application settlement.
type AttentionError struct{ Reason AttentionReason }

func (err *AttentionError) Error() string {
	return "retained output needs attention: " + string(err.Reason)
}

type Lookup struct {
	Attention   AttentionReason
	Kind        LookupKind
	Operation   Operation
	Reservation Reservation
}

func (lookup Lookup) Valid() bool {
	switch lookup.Kind {
	case OutputLookupMiss:
		return !lookup.Operation.Valid() && lookup.Reservation != nil
	case OutputLookupAlreadyRunning, OutputLookupNeedsAttention, OutputLookupAmbiguous:
		return !lookup.Operation.Valid()
	case OutputLookupReopened:
		return lookup.Operation.Valid()
	default:
		return false
	}
}

type Authority interface {
	BindDestination(context.Context) (Mode, error)
	LookupActive(context.Context, transfer.SelectionSpec) (Lookup, error)
	Close() error
}
type Config struct {
	Diagnostics diagnostics.Sink
}
type Factory interface {
	NewOutputAuthority(Config) (Authority, error)
}
type FactoryFunc func(Config) (Authority, error)

func (f FactoryFunc) NewOutputAuthority(c Config) (Authority, error) {
	if f == nil {
		return nil, ErrContract
	}
	return f(c)
}
