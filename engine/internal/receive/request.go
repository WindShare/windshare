// Package receive owns one receive operation across connection generations.
package receive

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/transport/relayv2"
)

const defaultCleanupTimeout = 5 * time.Second

var ErrMissingOutputFactory = errors.New("receive output factory is required")

var errInvalidRequest = errors.New("receive request is invalid")

// Request binds one destination authority to a selection. Reconnection never
// reacquires it: its reservation and authenticated progress belong to the task.
type OutputAdmission uint8

const (
	OutputCreateOrResume OutputAdmission = iota
	OutputResumeExisting
	OutputRedownload
)

type Request struct {
	Admission    OutputAdmission
	OperationID  receivecontract.OperationID
	Capability   link.Link
	Only         []string
	Connectivity ConnectivityPolicy
	WaitTimeout  time.Duration
	Output       OutputFactory
	Destination  string
	Diagnostics  diagnostics.Sink
	OnPause      func(context.Context, Pause) (RecoveryAction, error)
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
	Capacity     *liveshare.ReceiveCapacity
	Connectivity *nativeconnectivity.Capacity
	Control      task.Control
	Clock        Clock
	ReceiverDial func(context.Context, relayv2.ReceiverConfig) (relayset.ReceiverConnection, error)
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

type preparedRequest struct {
	admission    OutputAdmission
	operationID  receivecontract.OperationID
	outDir       string
	selection    transfer.SelectionRules
	link         link.Link
	connectivity receiverConnectivityPlan
	waitTimeout  time.Duration
	output       OutputFactory
}

// Freeze caller intent before acquiring resources so retries and connection
// generations cannot reinterpret the selection of an already-owned operation.
func prepareRequest(request Request) (preparedRequest, error) {
	if request.Capability.Suite != link.SuiteSenderAuthenticated || request.WaitTimeout < 0 || request.Admission > OutputRedownload || !request.OperationID.IsZero() && request.Admission != OutputResumeExisting {
		return preparedRequest{}, errInvalidRequest
	}
	connectivity, err := request.Connectivity.receiverPlan()
	if err != nil {
		return preparedRequest{}, err
	}
	rules, err := selectionRules(request.Only)
	if err != nil {
		return preparedRequest{}, err
	}
	return preparedRequest{
		admission: request.Admission, operationID: request.OperationID,
		outDir: request.Destination, selection: rules, link: request.Capability,
		connectivity: connectivity, waitTimeout: request.WaitTimeout, output: request.Output,
	}, nil
}

func selectionRules(requested []string) (transfer.SelectionRules, error) {
	if len(requested) == 0 {
		return transfer.NewSelectionRules(true, nil)
	}
	return transfer.NewPathSelectionRules(requested)
}

type runner struct {
	connectivityCapacity    *nativeconnectivity.Capacity
	receiveCapacity         *liveshare.ReceiveCapacity
	receiverRecoveryOptions relayset.ReceiverRecoveryOptions
	receiverPeerFactory     func() (receiverPeerStarter, error)
	receiverDial            func(context.Context, relayv2.ReceiverConfig) (relayset.ReceiverConnection, error)
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
