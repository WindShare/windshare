package lanes

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	MaxLogicalLanes     = 16
	MaxDemandAttempts   = MaxLogicalLanes
	maximumLaneFailures = 1_000
)

var (
	ErrInvalid         = errors.New("transfer lane is invalid")
	ErrInvalidRequest  = errors.New("block request requires a validator")
	ErrStale           = errors.New("transfer lane epoch is stale")
	ErrBudget          = errors.New("transfer lane budget exceeded")
	ErrClosed          = errors.New("transfer lane set is closed")
	ErrNoContentRoute  = errors.New("no permitted content route remains")
	errBlockSuperseded = errors.New("block attempt superseded by an authenticated winner")
)

type Identity struct {
	ID    uint32
	Epoch uint32
}

type Demand struct {
	LeaseID    content.LeaseID
	Descriptor content.FileRevisionDescriptor
	Index      uint64
}

type BlockSource interface {
	// Receipt reports unique sealed-object bytes before authentication. It may
	// replenish network supply, but never proves output durability.
	FetchBlock(context.Context, Demand, func(uint64)) (records.BlockRecord, error)
}

// BlockCapacitySource reserves response storage before any request is sent.
// A source without storage (for example, an in-memory source) needs no claim.
type BlockCapacitySource interface {
	TryReserveBlock(Demand) (BlockReservation, <-chan struct{}, error)
}

type BlockReservation struct {
	Source  BlockSource
	Release func()
}

// AttemptSuperseded proves that the demand owner canceled a redundant
// attempt after selecting an authenticated winner. Plain cancellation, including
// a parent failure, cannot supply this evidence to a lane's terminal diagnostics.
func AttemptSuperseded(ctx context.Context) bool {
	return ctx != nil && context.Cause(ctx) == errBlockSuperseded //nolint:errorlint // Only the lane round's exact cause grants supersession ownership.
}

type Config struct {
	ContentRoutePolicy            ContentRoutePolicy
	ProtocolSessionID             protocolsession.ProtocolSessionID
	Now                           func() time.Time
	After                         func(time.Duration) <-chan time.Time
	SettlementObservationCapacity SettlementObservationCapacity
}

// CapacityClaim reserves one physical attempt before its source is called.
// A nil release with a change signal means admission must wait. Release remains
// owned by the attempt until its plaintext is handed back or discarded.
type CapacityClaim func() (release func(), changed <-chan struct{}, err error)

// Request binds an immutable block demand to the consumer's validation and
// capacity authority. Validation must return a typed fault for known failures;
// lane scheduling cannot infer consumer policy from diagnostic error strings.
type Request struct {
	Demand   Demand
	Validate func(records.BlockRecord) error
	Claim    CapacityClaim
	Order    func() WorkOrder
	Ready    func() CapacityReadiness
}

type WorkPriority uint8

const (
	Download WorkPriority = iota
	Preview
	Prefetch
)

// Distance follows the consumer's output frontier; LastServed orders independent
// consumers fairly without reserving a fixed share of traffic for any of them.
type WorkOrder struct {
	Priority   WorkPriority
	Distance   uint64
	LastServed uint64
}

// Block retains the admitted route of the selected authenticated result.
// Retired incarnations carry Unknown so a late result cannot claim live routing.
type Block struct {
	records.BlockRecord
	route downloadmetrics.Route
}

func (block Block) Route() downloadmetrics.Route { return block.route }
