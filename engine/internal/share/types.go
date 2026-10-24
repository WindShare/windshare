// Package share owns native sender application orchestration.
package share

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

var ErrConfiguration = errors.New("invalid share workflow configuration")

type Request struct {
	Source         liveshare.FileSourceFactory
	Relays         []relayprofile.Candidate
	ChunkSize      uint32
	Diagnostics    bool
	TraceLifecycle bool
}

type Ready struct {
	Capability          link.Link
	SelectedRootSummary liveshare.SelectedRootSummary
	RelayEndpoint       v2.RelayEndpoint
}

type Result struct {
	Elapsed time.Duration
	// Ready records first service readiness, not availability at shutdown.
	Ready             bool
	ObservationLosses []ObservationLoss
}

// Prepared makes preparation and shutdown independently testable while the
// production adapter keeps the sender's unique runtime-factory authority.
type Prepared interface {
	AuthorizeRegistration() error
	Registration() liveshare.RegistrationMaterial
	Capability() link.Link
	SelectedRootSummary() liveshare.SelectedRootSummary
	NewRuntimeFactory(liveshare.RuntimeFactoryConfig) (SessionFactory, error)
	// StartRootPrefetch schedules optional work without waiting for a scan.
	StartRootPrefetch()
	Close() error
}

type SessionFactory interface {
	AdmitChannel(context.Context, protocolsession.FrameChannel) (sessionruntime.SenderChannelAdmission, error)
	// Stop freezes admission before returning and joins terminal work on success.
	// An expired wait must remain joinable through a subsequent Stop call.
	Stop(context.Context, string) error
}

type Relays interface {
	Accept(context.Context) (protocolsession.FrameChannel, error)
	WaitReady(context.Context) error
	ReadyRelayURL() string
	Wake()
	StopRecovery()
	Cleanup(context.Context) error
}

type Peers interface {
	sessionruntime.SenderPeerHandlerFactory
	Close(context.Context) error
}

type Relay interface {
	relayset.SenderEndpoint
	CompleteObservations() relayv2.LifecycleObservationCompletion
}

type Dependencies struct {
	TURN             turnpath.Accounts
	Control          task.Control
	Controller       *Controller
	RevisionCapacity *revisioncapacity.Coordinator
	CatalogBudget    *catalog.BudgetAccount
	CacheBudget      *contentflow.ProcessCacheBudget
	Connectivity     *nativeconnectivity.Capacity
	Prepare          func(context.Context, liveshare.SenderConfig) (Prepared, error)
	Relays           func(context.Context, []string, relayset.SenderFactory) (Relays, error)
	Relay            func(senderrelay.Config) (Relay, error)
	Peers            func(nativeconnectivity.SenderConfig) (Peers, error)
	PollNetwork      func(context.Context, func())
}

type nativePrepared struct{ *liveshare.PreparedSender }

func (p nativePrepared) NewRuntimeFactory(config liveshare.RuntimeFactoryConfig) (SessionFactory, error) {
	return p.PreparedSender.NewRuntimeFactory(config)
}

func (d Dependencies) normalized() Dependencies {
	if d.Prepare == nil {
		d.Prepare = func(ctx context.Context, config liveshare.SenderConfig) (Prepared, error) {
			p, err := liveshare.PrepareSender(ctx, config)
			if p == nil {
				return nil, err
			}
			return nativePrepared{p}, err
		}
	}
	if d.Relays == nil {
		d.Relays = func(ctx context.Context, urls []string, create relayset.SenderFactory) (Relays, error) {
			value, err := relayset.NewSender(ctx, urls, create)
			if value == nil {
				return nil, err
			}
			return nativeRelays{value}, err
		}
	}
	if d.Relay == nil {
		d.Relay = func(config senderrelay.Config) (Relay, error) { return senderrelay.New(config) }
	}
	if d.Peers == nil {
		d.Peers = func(config nativeconnectivity.SenderConfig) (Peers, error) {
			value, err := nativeconnectivity.NewSender(config)
			if value == nil {
				return nil, err
			}
			return nativePeers{value}, err
		}
	}
	if d.PollNetwork == nil {
		d.PollNetwork = wakeOnNetworkChange
	}
	return d
}

type nativeRelays struct{ *relayset.Sender }

func (relays nativeRelays) Accept(ctx context.Context) (protocolsession.FrameChannel, error) {
	channel, err := relays.Sender.Accept(ctx)
	if channel == nil {
		return nil, err
	}
	return channel, err
}

type nativePeers struct{ *v2peer.Factory }

func (peers nativePeers) Close(ctx context.Context) error {
	if peers.NativeConnectivity() == nil {
		return nil
	}
	return peers.NativeConnectivity().Close(ctx)
}
