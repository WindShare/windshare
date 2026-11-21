package engine

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/engine/turn"
)

type PreparedShare = share.Prepared
type ShareSessionFactory = share.SessionFactory
type ShareRelays = share.Relays
type SharePeers = share.Peers

// Dependencies expose construction seams; task control and aggregate budgets
// are assigned by the Engine so an injected constructor cannot replace another
// task's ownership.
type ShareDependencies struct {
	Prepare     func(context.Context, liveshare.SenderConfig) (PreparedShare, error)
	Relays      func(context.Context, []relayprofile.Registration) (ShareRelays, error)
	Peers       func() (SharePeers, error)
	PollNetwork func(context.Context, func())
}

type ReceiveDependencies struct {
	Clock        ReceiveClock
	ReceiverDial func(context.Context, ReceiveDialRequest) (ReceiveConnection, error)
	Recovery     ReceiveRecoverySettings
	PeerFactory  func() (ReceivePeerStarter, error)
}

// Clock keeps task timestamps, sender registrations, and node admissions in the same time domain.
type Clock interface {
	Now() time.Time
	// Wait ends when the delay elapses, wake signals a change, or ctx is canceled.
	Wait(ctx context.Context, delay time.Duration, wake <-chan struct{}) error
}

type Config struct {
	Network *networkconfig.Config
	// Diagnostics receives engine-scoped infrastructure events (such as
	// directory refreshes and frontend health checks) bound with Scope.EngineID.
	Diagnostics         diagnostics.Sink
	TURN                turn.Settings
	TURNDependencies    turn.Dependencies
	RelaySetup          relaysetup.Dependencies
	Clock               Clock
	Random              io.Reader
	CleanupTimeout      time.Duration
	ObservationCapacity observationstream.Capacity
	RevisionCapacity    revisioncapacity.ProcessConfig
	CatalogLimits       catalog.BudgetLimits
	CacheBytes          uint64
	ReceiveCapacity     liveshare.ReceiveCapacityLimits
	Share               ShareDependencies
	Receive             ReceiveDependencies
}
