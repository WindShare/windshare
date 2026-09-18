// Package receive owns one receive operation across connection generations.
package receive

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/transport/relayv2"
)

const defaultCleanupTimeout = 5 * time.Second

var ErrMissingOutputFactory = errors.New("receive output factory is required")

// Request binds one destination authority to a selection. Reconnection never
// reacquires it: its reservation and authenticated progress belong to the task.
type Request struct {
	Capability   link.Link
	Only         []string
	Connectivity ConnectivityPolicy
	WaitTimeout  time.Duration
	Output       OutputFactory
	Destination  string
	Diagnostics  bool
}

type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}
type Ticker interface {
	C() <-chan time.Time
	Stop()
}
type systemClock struct{ now func() time.Time }

func (c systemClock) Now() time.Time                   { return c.now() }
func (c systemClock) NewTicker(d time.Duration) Ticker { return systemTicker{time.NewTicker(d)} }

type systemTicker struct{ ticker *time.Ticker }

func (t systemTicker) C() <-chan time.Time { return t.ticker.C }
func (t systemTicker) Stop()               { t.ticker.Stop() }

type Dependencies struct {
	Control      task.Control
	Clock        Clock
	ReceiverDial func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error)
	Recovery     relayset.ReceiverRecoveryOptions
	PeerFactory  func() (PeerStarter, error)
}

func (d Dependencies) normalized() Dependencies {
	if d.Control.Random == nil {
		d.Control.Random = rand.Reader
	}
	if d.Control.Now == nil {
		d.Control.Now = time.Now
	}
	if d.Control.CleanupContext == nil {
		d.Control.CleanupContext = func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), defaultCleanupTimeout)
		}
	}
	if d.Clock == nil {
		d.Clock = systemClock{now: d.Control.Now}
	}
	return d
}

type getRequest struct {
	outDir       string
	only         []string
	link         link.Link
	connectivity ConnectivityPolicy
	waitTimeout  time.Duration
}

type runner struct {
	receiverRecoveryOptions relayset.ReceiverRecoveryOptions
	receiverPeerFactory     func() (receiverPeerStarter, error)
	receiverDial            func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error)
	getOutputFactory        getOutputAuthorityFactory
	control                 task.Control
	clock                   Clock
}

type stepOutcome uint8

const (
	stepReady stepOutcome = iota
	stepLocalFailure
	stepInvalidRequest
	stepNetworkFailure
)

// Aliases expose only the caller-owned injection seams, not lifecycle helpers.
type PeerStarter = receiverPeerStarter
type PeerAttempt = receiverPeerAttempt
type PeerOutcome = receiverPeerMonitorOutcome
type PeerDisposition = receiverPeerDisposition

const (
	PeerFallbackAllowed    = receiverPeerFallbackAllowed
	PeerSessionUnavailable = receiverPeerSessionUnavailable
	PeerSessionUnsafe      = receiverPeerSessionUnsafe
	PeerLocalStop          = receiverPeerLocalStop
)

func NewPeerOutcome(disposition PeerDisposition, cause error) PeerOutcome {
	return receiverPeerMonitorOutcome{disposition: disposition, retainedCause: cause}
}
