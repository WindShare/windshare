package engine

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/engine/internal/share"
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
	Relays      func(context.Context, []relayprofile.Candidate) (ShareRelays, error)
	Peers       func() (SharePeers, error)
	PollNetwork func(context.Context, func())
}

type ReceiveDependencies struct {
	Clock        ReceiveClock
	ReceiverDial func(context.Context, ReceiveDialRequest) (ReceiveConnection, error)
	Recovery     ReceiveRecoverySettings
	PeerFactory  func() (ReceivePeerStarter, error)
}

type Config struct {
	TURN                turn.Settings
	TURNDependencies    turn.Dependencies
	RelaySetup          relaysetup.Dependencies
	Now                 func() time.Time
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
