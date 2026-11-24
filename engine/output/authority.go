// Package output owns destination acquisition and receive output contracts.
package output

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const MaxNameAdjustmentSamples = 20

// NameAdjustment records one logical name or path saved under a compatible
// physical spelling: relative to the destination directory for nested items, or
// the destination entry name itself when the top-level root or single file was
// repaired.
type NameAdjustment struct {
	LogicalPath  string
	PhysicalPath string
}

// NameAdjustmentSummary is a bounded value snapshot of compatible-name repairs
// materialized by an output backend. Count is exact; Samples is capped.
type NameAdjustmentSummary struct {
	Count   uint64
	Samples []NameAdjustment
}

func (summary NameAdjustmentSummary) Empty() bool {
	return summary.Count == 0 && len(summary.Samples) == 0
}

func (summary NameAdjustmentSummary) Valid() bool {
	if summary.Count == 0 {
		return len(summary.Samples) == 0
	}
	if len(summary.Samples) == 0 || uint64(len(summary.Samples)) > summary.Count || len(summary.Samples) > MaxNameAdjustmentSamples {
		return false
	}
	for _, sample := range summary.Samples {
		if sample.LogicalPath == "" || sample.PhysicalPath == "" || sample.LogicalPath == sample.PhysicalPath {
			return false
		}
	}
	return true
}

func (summary NameAdjustmentSummary) Clone() NameAdjustmentSummary {
	return NameAdjustmentSummary{
		Count:   summary.Count,
		Samples: slices.Clone(summary.Samples),
	}
}

var ErrContract = errors.New("receive output authority violated its contract")
var ErrShareStopped = errors.New("retained output belongs to a stopped share")
var ErrOperationAlreadyRunning = errors.New("receive output operation is already running")
var ErrOperationAmbiguous = errors.New("receive output operation ownership is ambiguous")

// A missing explicit selection cannot acquire permission to create a new
// download. Only CreateReady carries that reservation authority.
type LookupKind uint8

const (
	OutputLookupCreateReady LookupKind = iota + 1
	OutputLookupNotFound
	OutputLookupReopened
	OutputLookupAlreadyRunning
	OutputLookupNeedsAttention
	OutputLookupAmbiguous
)

// Operation couples immutable intent with the authority that can open it.
// It does not expose a filesystem path or a provider-specific operation handle.
type OperationControl interface {
	// Idle reports whether this binding still owns the operation between output sessions.
	Idle() bool
	RecordShareStopped(context.Context) error
	Discard(context.Context) error
	// NameAdjustments returns a bounded value snapshot of compatible-name
	// repairs made for this operation. It remains readable after terminal close.
	NameAdjustments() NameAdjustmentSummary
}

type Operation struct {
	Intent       transfer.ReceiveIntent
	Materializer transfer.DirectTreeMaterializer
	Control      OperationControl
	// Destination is an optional provider-owned display label, never output authority.
	Destination         string
	DestinationAdjusted bool
}

func (o Operation) Valid() bool {
	return !o.Intent.IsZero() && o.Materializer != nil
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
	case OutputLookupCreateReady:
		return !lookup.Operation.Valid() && lookup.Reservation != nil
	case OutputLookupNotFound, OutputLookupAlreadyRunning, OutputLookupNeedsAttention, OutputLookupAmbiguous:
		return !lookup.Operation.Valid() && lookup.Reservation == nil
	case OutputLookupReopened:
		return lookup.Operation.Valid() && lookup.Reservation == nil
	default:
		return false
	}
}

type Authority interface {
	BindDestination(context.Context) error
	SelectOperation(context.Context, transfer.SelectionSpec, receivecontract.OperationSelection) (Lookup, error)
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
