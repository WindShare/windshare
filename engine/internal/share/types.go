// Package share owns native sender application orchestration.
package share

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
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
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

var ErrConfiguration = errors.New("invalid share workflow configuration")
var ErrPublication = errors.New("share capability publication failed")

type PublicationStage uint8

const (
	PublicationEncoding PublicationStage = iota + 1
	PublicationOutput
	PublicationReadiness
)

// PublicationError reports the adapter's failing operation without deciding the
// application outcome or exposing its terminal representation to the engine.
type PublicationError struct {
	Stage PublicationStage
	Cause error
}

func (e *PublicationError) Error() string { return ErrPublication.Error() }
func (e *PublicationError) Unwrap() error { return e.Cause }

type Request struct {
	Source         liveshare.FileSourceFactory
	RelayURLs      []string
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
	Elapsed           time.Duration
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
	Accept(context.Context) (*relayv2.Channel, error)
	WaitReady(context.Context) error
	ReadyRelayURL() string
	ObserveAvailability(func(relayset.SenderAvailability))
	Wake()
	StopRecovery()
	Cleanup(context.Context) error
}

type Relay interface {
	relayset.SenderEndpoint
	CompleteObservations() relayv2.LifecycleObservationCompletion
}

type Dependencies struct {
	Control          task.Control
	Controller       *Controller
	RevisionCapacity *revisioncapacity.Coordinator
	CatalogBudget    *catalog.BudgetAccount
	CacheBudget      *contentflow.ProcessCacheBudget
	Prepare          func(context.Context, liveshare.SenderConfig) (Prepared, error)
	Relays           func(context.Context, []string, relayset.SenderFactory) (Relays, error)
	Relay            func(senderrelay.Config) (Relay, error)
	Peers            func(nativeconnectivity.SenderConfig) (*v2peer.Factory, error)
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
			return relayset.NewSender(ctx, urls, create)
		}
	}
	if d.Relay == nil {
		d.Relay = func(config senderrelay.Config) (Relay, error) { return senderrelay.New(config) }
	}
	if d.Peers == nil {
		d.Peers = nativeconnectivity.NewSender
	}
	if d.PollNetwork == nil {
		d.PollNetwork = wakeOnNetworkChange
	}
	return d
}
