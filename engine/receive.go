package engine

import (
	"context"

	"github.com/windshare/windshare/core/osfs"

	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
)

type ReceiveRequest = receive.Request
type ReceiveResult = receive.Result
type ReceiveSettlementInput = receive.SettlementInput
type ReceiveFailure = receive.Failure
type ReceiveFailureCode = receive.FailureCode
type ReceiveLocalFailure = receive.LocalFailure
type ReceiveClock = receive.Clock
type ReceiveTicker = receive.Ticker
type ReceivePeerStarter = receive.PeerStarter
type ReceivePeerAttempt = receive.PeerAttempt
type ReceivePeerOutcome = receive.PeerOutcome
type ReceivePeerDisposition = receive.PeerDisposition
type ConnectivityPolicy = receive.ConnectivityPolicy

type OutputFactory = receive.OutputFactory
type OutputFactoryFunc = receive.OutputFactoryFunc
type OutputConfig = receive.OutputConfig
type OutputAuthority = receive.OutputAuthority
type OutputMode = receive.OutputMode
type OutputLookup = receive.OutputLookup
type OutputLookupKind = receive.OutputLookupKind
type OutputReservation = receive.OutputReservation
type OutputOperation = receive.OutputOperation
type FilesystemOutput = receive.FilesystemOutput

const (
	ConnectivityAuto                 = receive.ConnectivityAuto
	ConnectivityRelayOnly            = receive.ConnectivityRelayOnly
	ConnectivityP2POnly              = receive.ConnectivityP2POnly
	ReceivePeerFallbackAllowed       = receive.PeerFallbackAllowed
	ReceivePeerSessionUnavailable    = receive.PeerSessionUnavailable
	ReceivePeerSessionUnsafe         = receive.PeerSessionUnsafe
	ReceivePeerLocalStop             = receive.PeerLocalStop
	OutputResumable                  = receive.OutputResumable
	OutputLiveOnly                   = receive.OutputLiveOnly
	OutputLookupMiss                 = receive.OutputLookupMiss
	OutputLookupReopened             = receive.OutputLookupReopened
	OutputLookupAlreadyRunning       = receive.OutputLookupAlreadyRunning
	OutputLookupNeedsAttention       = receive.OutputLookupNeedsAttention
	OutputLookupAmbiguous            = receive.OutputLookupAmbiguous
	ReceiveLocalFailureNone          = receive.LocalFailureNone
	ReceiveLocalSelectionMissing     = receive.LocalSelectionMissing
	ReceiveLocalRevisionConflict     = receive.LocalRevisionConflict
	ReceiveLocalCheckpointInvalid    = receive.LocalCheckpointInvalid
	ReceiveLocalOwnedObjectUnknown   = receive.LocalOwnedObjectUnknown
	ReceiveLocalDestinationCollision = receive.LocalDestinationCollision
)

var ErrInvalidConnectivityPolicy = receive.ErrInvalidConnectivityPolicy
var ErrInvalidReceiveResult = receive.ErrInvalidResult

type ReceiveTask struct{ *Task[ReceiveResult] }

func (engine *Engine) StartReceive(ctx context.Context, request ReceiveRequest) (*ReceiveTask, error) {
	// Admission transfers value ownership before asynchronous work can outlive its caller.
	request.Only = append([]string(nil), request.Only...)
	request.Capability.ReadSecret = append([]byte(nil), request.Capability.ReadSecret...)
	request.Capability.PKHash = append([]byte(nil), request.Capability.PKHash...)
	request.Capability.Relays = append([]string(nil), request.Capability.Relays...)
	injected := engine.config.Receive
	dependencies := receive.Dependencies{
		Clock: injected.Clock, ReceiverDial: injected.ReceiverDial,
		Recovery: injected.Recovery, PeerFactory: injected.PeerFactory,
	}
	current, err := start(engine, ctx, func(ctx context.Context, control task.Control) task.Completion[receive.Result] {
		dependencies.Control = control
		return receive.Run(ctx, request, dependencies)
	})
	if err != nil {
		return nil, err
	}
	return &ReceiveTask{Task: current}, nil
}

func ParseConnectivityPolicy(value string) (ConnectivityPolicy, error) {
	return receive.ParseConnectivityPolicy(value)
}

func NewReceivePeerOutcome(disposition ReceivePeerDisposition, cause error) ReceivePeerOutcome {
	return receive.NewPeerOutcome(disposition, cause)
}

func SettleReceive(input ReceiveSettlementInput) (TaskCompletion[ReceiveResult], error) {
	return receive.Settle(input)
}

func ReceiveOutputDiagnostic(cause error) (osfs.FilesystemOutputDiagnostic, bool) {
	return receive.OutputDiagnostic(cause)
}
