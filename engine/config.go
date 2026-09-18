package engine

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/transport/relayv2"
)

type PreparedShare = share.Prepared
type ShareSessionFactory = share.SessionFactory
type ShareRelays = share.Relays
type ShareRelay = share.Relay
type SenderPeerConfig = nativeconnectivity.SenderConfig

// Dependencies expose construction seams; task control and aggregate budgets
// are assigned by the Engine so an injected constructor cannot replace another
// task's ownership.
type ShareDependencies struct {
	Prepare     func(context.Context, liveshare.SenderConfig) (PreparedShare, error)
	Relays      func(context.Context, []string, relayset.SenderFactory) (ShareRelays, error)
	Relay       func(senderrelay.Config) (ShareRelay, error)
	Peers       func(SenderPeerConfig) (*v2peer.Factory, error)
	PollNetwork func(context.Context, func())
}

type ReceiveDependencies struct {
	Clock        ReceiveClock
	ReceiverDial func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error)
	Recovery     relayset.ReceiverRecoveryOptions
	PeerFactory  func() (ReceivePeerStarter, error)
}

type Config struct {
	Now                 func() time.Time
	Random              io.Reader
	CleanupTimeout      time.Duration
	ObservationCapacity observationstream.Capacity
	RevisionCapacity    revisioncapacity.ProcessConfig
	CatalogLimits       catalog.BudgetLimits
	CacheBytes          uint64
	Share               ShareDependencies
	Receive             ReceiveDependencies
}
