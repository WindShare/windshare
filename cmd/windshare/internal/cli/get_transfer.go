package cli

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/transport/relayv2"
)

var (
	errGetOutputOperationAlreadyRunning = errors.New("get output operation is already running")
	errGetOutputOperationNeedsAttention = errors.New("get output operation needs attention")
	errGetOutputOperationAmbiguous      = errors.New("get output operation ownership is ambiguous")
	errGetOutputReservationContract     = errors.New("get output operation reservation violated its contract")
)

type getOutputPreparation struct {
	authority   getOutputAuthority
	mode        getOutputMode
	displayRoot string
	clock       receiverAdmissionClock
	startedAt   time.Time
}

func (a *App) prepareGetOutput(
	ctx context.Context,
	request getRequest,
	observation getObservation,
) (getOutputPreparation, int) {
	outputRoot, err := filepath.Abs(request.outDir)
	if err != nil {
		return getOutputPreparation{}, observation.commandFailure(ExitFailure, err)
	}
	clock := a.admissionClock()
	// Starting the command certifies the caller-provided container. The operation
	// identity is resolved later, after selection is frozen, so a repeated command
	// can reopen exactly one compatible owned reservation.
	startedAt := clock.Now()
	factory := a.getOutputFactory
	if factory == nil {
		factory = getOutputAuthorityFactoryFunc(newFilesystemGetOutputAuthority)
	}
	authority, err := factory.NewGetOutputAuthority(getOutputAuthorityConfig{
		rootPath: outputRoot, createRoot: true,
		tracer: osfs.FilesystemOutputTraceFunc(observation.filesystemOutput),
	})
	if err != nil {
		return getOutputPreparation{}, observation.commandFailure(ExitFailure, err)
	}
	mode, err := authority.BindDestination(ctx)
	if err != nil {
		closeErr := authority.Close()
		return getOutputPreparation{}, observation.commandFailure(ExitFailure, errors.Join(err, closeErr))
	}
	return getOutputPreparation{
		authority: authority, mode: mode, displayRoot: outputRoot,
		clock: clock, startedAt: startedAt,
	}, ExitOK
}

type getReceiverSession struct {
	connection *relayv2.ReceiverConnection
	prepared   *liveshare.PreparedReceiver
	runtime    *sessionruntime.ReceiverRuntime
}

func (session *getReceiverSession) Close() {
	if session == nil {
		return
	}
	if session.runtime != nil {
		session.runtime.Close()
	}
	if session.prepared != nil {
		session.prepared.Close()
	}
	if session.connection != nil {
		_ = session.connection.Close()
	}
}

func (a *App) connectGetReceiver(
	ctx context.Context,
	capability link.Link,
	observation getObservation,
) (*getReceiverSession, int) {
	connection, code := a.dialV2Receiver(ctx, capability, observation)
	if code != ExitOK {
		return nil, code
	}
	prepared, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
		Capability: capability, DescriptorObject: connection.Descriptor(),
		PeerControls: v2signal.ReceiverControlValidator{},
	})
	if err != nil {
		_ = connection.Close()
		return nil, observation.commandFailureCode(ExitUsage, clievent.FailureCapabilityInvalid)
	}
	runtime, err := prepared.Connect(ctx, connection.Channel())
	if err != nil {
		prepared.Close()
		_ = connection.Close()
		if ctx.Err() != nil {
			return nil, observation.commandFailure(ExitFailure, ctx.Err())
		}
		return nil, observation.commandFailure(ExitNetwork, err)
	}
	return &getReceiverSession{
		connection: connection, prepared: prepared, runtime: runtime,
	}, ExitOK
}

type getTransferExecution struct {
	runtime             *sessionruntime.ReceiverRuntime
	admission           receiverContentAdmission
	monitorDone         <-chan struct{}
	peer                *activeReceiverPeer
	operation           getOutputOperation
	destination         string
	destinationAdjusted bool
	job                 *transfer.TransferJob
	settled             bool
	closed              bool
}

func (execution *getTransferExecution) Close() {
	if execution == nil || execution.closed {
		return
	}
	execution.closed = true
	if execution.peer != nil {
		execution.peer.Close()
	}
	execution.SettleAdmission()
}

func (execution *getTransferExecution) SettleAdmission() {
	if execution == nil || execution.settled {
		return
	}
	execution.settled = true
	execution.admission.Close()
	execution.admission.Wait()
	<-execution.monitorDone
}

func (a *App) prepareGetTransfer(
	ctx context.Context,
	request getRequest,
	output getOutputPreparation,
	runtime *sessionruntime.ReceiverRuntime,
	observation getObservation,
) (*getTransferExecution, int) {
	connectivity, err := request.connectivity.receiverPlan()
	if err != nil {
		return nil, observation.commandFailureCode(ExitUsage, clievent.FailureInvalidInput)
	}
	laneID, laneEpoch := runtime.LaneIdentity()
	relaySuspension, err := runtime.LaneSet().SuspendContent(
		transfer.LaneIdentity{ID: laneID, Epoch: laneEpoch},
	)
	if err != nil {
		return nil, observation.commandFailure(ExitFailure, err)
	}
	contentReady := make(chan struct{})
	paths := newReceiverContentPaths(observation)
	admission, err := newReceiverContentAdmissionWithExecution(
		connectivity.relayContent,
		output.startedAt,
		output.clock,
		relaySuspension,
		receiverAdmissionExecution{
			claimGate: contentReady,
			onClaim: func(trigger receiverAdmissionTrigger) {
				a.observeRelayContentAdmission(trigger, paths)
			},
		},
	)
	if err != nil {
		return nil, observation.commandFailure(ExitFailure, err)
	}
	execution := &getTransferExecution{
		runtime: runtime, admission: admission,
		monitorDone: a.monitorReceiverAdmission(admission, runtime, observation),
	}
	observePeer := func(signal receiverPeerSignal) {
		paths.observePeer(signal)
		if observeErr := admission.ObservePeer(signal); observeErr != nil {
			observation.warning(observeErr)
			runtime.Close()
		}
	}
	peer, rules, err := beginReceiverPlanning(
		connectivity,
		func() *activeReceiverPeer { return a.startReceiverPeer(ctx, runtime, observation, observePeer) },
		admission.AdmitRelayOnly,
		func() (transfer.SelectionRules, error) { return selectionRules(request.only) },
	)
	execution.peer = peer
	if err != nil {
		if errors.Is(err, errReceiverP2PPathUnavailable) {
			observation.commandFailureCode(ExitNetwork, clievent.FailurePeerNegotiation)
			execution.Close()
			return nil, ExitNetwork
		}
		observation.commandFailure(ExitUsage, err)
		execution.Close()
		return nil, ExitUsage
	}
	job, operation, destination, adjusted, code := a.buildGetTransferJob(
		ctx, runtime, output, rules, observation,
	)
	if code != ExitOK {
		execution.Close()
		if errors.Is(admission.Err(), errReceiverP2PPathUnavailable) {
			return nil, ExitNetwork
		}
		return nil, code
	}
	if output.mode == getOutputLiveOnly {
		observation.warningCode(clievent.FailureOutputUnsupportedFilesystem)
	}
	execution.operation = operation
	execution.destination = destination
	execution.destinationAdjusted = adjusted
	execution.job = job
	// Lane timing may queue relay admission while shape and destination authority
	// are being resolved. Releasing this separate gate only after the immutable
	// operation and job exist prevents any content request from outrunning them.
	close(contentReady)
	return execution, ExitOK
}

func (a *App) buildGetTransferJob(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	output getOutputPreparation,
	rules transfer.SelectionRules,
	observation getObservation,
) (*transfer.TransferJob, getOutputOperation, string, bool, int) {
	selection, err := transfer.NewSelectionSpec(
		runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.commandFailure(ExitFailure, err)
	}
	admission, err := resolveGetOutputOperation(ctx, output.authority, runtime, selection)
	if err != nil {
		return nil, getOutputOperation{}, "", false, reportGetOutputAdmissionFailure(observation, err)
	}
	if admission.operation.mode != output.mode {
		return nil, getOutputOperation{}, "", false,
			reportGetOutputAdmissionFailure(observation, errGetOutputReservationContract)
	}
	destination, adjusted, err := getOperationDestination(output.displayRoot, admission.operation)
	if err != nil {
		return nil, getOutputOperation{}, "", false, reportGetOutputAdmissionFailure(observation, err)
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.commandFailure(ExitFailure, err)
	}
	job, err := runtime.NewTransferJob(
		admission.operation.intent,
		jobID,
		getOperationMaterializer{authority: output.authority, operation: admission.operation},
		transfer.TransferLifecycleTraceFunc(observation.transferLifecycle),
	)
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.commandFailure(ExitFailure, err)
	}
	return job, admission.operation, destination, adjusted, ExitOK
}

type getShapeResolver interface {
	ResolveOrdinaryOutputShape(
		context.Context,
		transfer.SelectionSpec,
		ordinaryoutput.ShapeProbeBudget,
		ordinaryoutput.ShapeTracer,
	) (ordinaryoutput.ShapeDecision, error)
}

type getOutputAdmission struct {
	operation getOutputOperation
	lookup    getOutputLookupKind
}

func getOperationDestination(
	displayRoot string,
	operation getOutputOperation,
) (string, bool, error) {
	if displayRoot == "" || !filepath.IsAbs(displayRoot) || !operation.valid() {
		return "", false, errGetOutputReservationContract
	}
	reservation, direct := operation.intent.MaterializationPlan().DestinationReservation()
	if !direct || reservation.IsZero() {
		return "", false, errGetOutputReservationContract
	}
	switch reservation.Kind() {
	case receivecontract.ReservationContainerRoot:
		return displayRoot, false, nil
	case receivecontract.ReservationNamedContainerEntry:
		if reservation.ReservedName() == "" {
			return "", false, errGetOutputReservationContract
		}
		return filepath.Join(displayRoot, reservation.ReservedName()), reservation.CollisionIndex() > 0, nil
	default:
		return "", false, errGetOutputReservationContract
	}
}

func resolveGetOutputOperation(
	ctx context.Context,
	authority getOutputAuthority,
	resolver getShapeResolver,
	selection transfer.SelectionSpec,
) (getOutputAdmission, error) {
	if ctx == nil || authority == nil || resolver == nil || selection.IsZero() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil {
		return getOutputAdmission{}, err
	}
	if !lookup.valid() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	var operation getOutputOperation
	switch lookup.kind {
	case getOutputLookupMiss:
		decision, resolveErr := resolver.ResolveOrdinaryOutputShape(
			ctx, selection, ordinaryoutput.DefaultShapeProbeBudgetV1, nil,
		)
		if resolveErr != nil {
			return getOutputAdmission{}, resolveErr
		}
		artifact, materializeErr := transfer.MaterializeOrdinaryOutputShape(decision)
		if materializeErr != nil {
			return getOutputAdmission{}, materializeErr
		}
		operation, err = authority.CreateOperation(ctx, lookup, artifact)
		if err != nil {
			return getOutputAdmission{}, err
		}
	case getOutputLookupReopened:
		operation = lookup.operation
	case getOutputLookupAlreadyRunning:
		return getOutputAdmission{}, errGetOutputOperationAlreadyRunning
	case getOutputLookupNeedsAttention:
		return getOutputAdmission{}, errGetOutputOperationNeedsAttention
	case getOutputLookupAmbiguous:
		return getOutputAdmission{}, errGetOutputOperationAmbiguous
	default:
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	if !operation.valid() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	reservation, direct := operation.intent.MaterializationPlan().DestinationReservation()
	if !direct || reservation.IsZero() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	return getOutputAdmission{
		operation: operation, lookup: lookup.kind,
	}, nil
}

func reportGetOutputAdmissionFailure(observation getObservation, err error) int {
	switch {
	case errors.Is(err, errGetOutputOperationAlreadyRunning):
		return observation.commandFailureCode(ExitFailure, clievent.FailureOutputFileAlreadyActive)
	case errors.Is(err, errGetOutputOperationNeedsAttention):
		return observation.commandFailureCode(ExitFailure, clievent.FailureCheckpointStateIO)
	case errors.Is(err, errGetOutputOperationAmbiguous):
		return observation.commandFailureCode(ExitFailure, clievent.FailureOutputOwnership)
	case errors.Is(err, errGetOutputReservationContract), errors.Is(err, errGetOutputAdapterContract):
		return observation.commandFailureCode(ExitFailure, clievent.FailureOutputContract)
	default:
		return observation.commandFailure(ExitFailure, err)
	}
}
