package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

var (
	errGetOutputOperationAlreadyRunning = errors.New("get output operation is already running")
	errGetOutputOperationNeedsAttention = errors.New("get output operation needs attention")
	errGetOutputOperationAmbiguous      = errors.New("get output operation ownership is ambiguous")
	errGetOutputReservationContract     = errors.New("get output operation reservation violated its contract")
)

type getOutputPreparation struct {
	contentReady chan struct{}
	paths        *receiverContentPaths
	authority    getOutputAuthority
	mode         getOutputMode
	displayRoot  string
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
	// Starting the command certifies the caller-provided container. The operation
	// identity is resolved later, after selection is frozen, so a repeated command
	// can reopen exactly one compatible owned reservation.
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
		contentReady: make(chan struct{}), paths: newReceiverContentPaths(observation),
	}, ExitOK
}

type getReceiverSession struct {
	relays     *relayset.Receiver
	connection *relayv2.ReceiverConnection
	prepared   *liveshare.PreparedReceiver
	runtime    *sessionruntime.ReceiverRuntime
}

func (session *getReceiverSession) Close() {
	if session == nil {
		return
	}
	if session.relays != nil {
		session.relays.Close()
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
	policy ConnectivityPolicy,
	observation getObservation,
) (*getReceiverSession, int) {
	session, err := a.openGetReceiver(ctx, capability, policy, observation)
	if err != nil {
		return nil, observation.commandFailure(ExitNetwork, err)
	}
	return session, ExitOK
}

func (a *App) openGetReceiver(ctx context.Context, capability link.Link, policy ConnectivityPolicy, observation getObservation, join ...context.Context) (*getReceiverSession, error) {
	set, err := relayset.NewReceiver(ctx, relayset.ReceiverConfig{
		Dial: a.receiverDial,
		Receiver: liveshare.ReceiverConfig{
			Capability: capability, ContentRoutePolicy: receiverRoutePolicy(policy),
			PeerControls: v2signal.ReceiverControlValidator{}, ProtocolTracer: observation.protocolTracer(),
			LaneSettlementObservationCapacity: observation.laneSettlementObservationCapacity(),
		},
		DialOptions: relayv2.DialOptions{LifecycleObservationCapacity: observation.relayObservationCapacity()},
		Connected: func(connection *relayv2.ReceiverConnection) {
			observation.registerRelayConnection(connection)
			observation.relayConnected(connection.Endpoint())
		},
	})
	if err != nil {
		return nil, err
	}
	wait := ctx
	if len(join) != 0 {
		wait = join[0]
	}
	runtime, connection, err := set.WaitReady(wait)
	if err != nil {
		set.Close()
		var rejection *relayv2.RelayError
		var joined *relayset.ReceiverJoinFailure
		if errors.As(err, &joined) && len(joined.RetryEndpoints()) == 0 && errors.As(err, &rejection) && rejection.Code == v2.ErrorStopped {
			a.recordProcessTrace(processTraceGetComponent, processTraceReceiverJoinStopped, testrun.OutcomeFailed)
		}
		return nil, err
	}
	observation.registerLaneSet(runtime.LaneSet())
	return &getReceiverSession{relays: set, connection: connection, runtime: runtime}, nil
}

type getTransferExecution struct {
	contentReady        chan struct{}
	paths               *receiverContentPaths
	runtime             *sessionruntime.ReceiverRuntime
	admission           receiverContentAdmission
	monitorDone         <-chan struct{}
	peer                *activeReceiverPeer
	localStop           *receiverLocalStop
	operation           getOutputOperation
	destination         string
	destinationAdjusted bool
	job                 *transfer.TransferJob
	settled             bool
	closed              bool
}

func (execution *getTransferExecution) inheritTransfer(previous *getTransferExecution) {
	execution.job = previous.job
	execution.destination = previous.destination
	execution.destinationAdjusted = previous.destinationAdjusted
}

func (execution *getTransferExecution) Close() {
	execution.CloseWithReason(clievent.ReceiverLocalStopCaller)
}

func (execution *getTransferExecution) CloseWithReason(reason clievent.ReceiverLocalStopReason) {
	if execution == nil || execution.closed {
		return
	}
	execution.closed = true
	if execution.peer != nil {
		execution.peer.CloseWithReason(reason)
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

func (a *App) prepareGetConnectivity(ctx context.Context, request getRequest, output getOutputPreparation, runtime *sessionruntime.ReceiverRuntime, observation getObservation, options receiverPeerOptions) (*getTransferExecution, int) {
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
	contentReady := output.contentReady
	if contentReady == nil {
		contentReady = make(chan struct{})
	}
	paths := output.paths
	if paths == nil {
		paths = newReceiverContentPaths(observation)
	}
	admission, err := newReceiverContentAdmissionWithExecution(
		connectivity.relayContent,
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
	localStop := &receiverLocalStop{}
	execution := &getTransferExecution{
		runtime: runtime, admission: admission, contentReady: contentReady, paths: paths,
		monitorDone: a.monitorReceiverAdmission(admission, runtime, observation, localStop),
		localStop:   localStop,
	}
	observePeer := func(signal receiverPeerSignal) {
		if observeErr := admission.ObservePeer(signal); observeErr != nil {
			observation.warning(observeErr)
			localStop.record(clievent.ReceiverLocalStopOutputAdmission)
			runtime.Close()
		}
	}
	peer, _, err := beginReceiverPlanning(
		connectivity,
		func() *activeReceiverPeer {
			options.demand = peerset.BrowseDemand
			select {
			case <-contentReady:
				options.demand = peerset.ContentDemand
			default:
			}
			return a.startReceiverPeer(ctx, runtime, observation, observePeer, localStop, connectivity.peer, options)
		},
		admission.AdmitRelayOnly,
		func() (transfer.SelectionRules, error) { return selectionRules(request.only) },
	)
	execution.peer = peer
	if err != nil {
		if errors.Is(err, errReceiverP2PPathUnavailable) {
			observation.commandFailureCode(ExitNetwork, clievent.FailurePeerNegotiation)
			execution.CloseWithReason(clievent.ReceiverLocalStopRuntimeSessionFailure)
			return nil, ExitNetwork
		}
		observation.commandFailure(ExitUsage, err)
		execution.CloseWithReason(clievent.ReceiverLocalStopCaller)
		return nil, ExitUsage
	}
	return execution, ExitOK
}

type getTransferDependencies interface {
	getShapeResolver
	NewTransferJob(transfer.ReceiveIntent, transfer.TransferJobID, transfer.DirectTreeMaterializer, transfer.TransferLifecycleTracer) (*transfer.TransferJob, error)
}

func (a *App) finishGetTransfer(ctx context.Context, request getRequest, output getOutputPreparation, dependencies getTransferDependencies, execution *getTransferExecution, observation getObservation) (*getTransferExecution, int) {
	rules, err := selectionRules(request.only)
	if err != nil {
		execution.Close()
		return nil, observation.commandFailure(ExitUsage, err)
	}
	job, operation, destination, adjusted, code := a.buildGetTransferJob(
		ctx, execution.runtime, output, rules, observation, dependencies,
	)
	if code != ExitOK {
		execution.CloseWithReason(clievent.ReceiverLocalStopOutputAdmission)
		if errors.Is(execution.admission.Err(), errReceiverP2PPathUnavailable) {
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
	if observation.state != nil && observation.state.downloadMetrics != nil {
		observation.state.downloadMetrics.Activate(fmt.Sprintf("%x", job.JobID().Bytes()))
	}
	close(execution.contentReady)
	execution.peer.SetDemand(peerset.ContentDemand)
	return execution, ExitOK
}

func (a *App) buildGetTransferJob(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	output getOutputPreparation,
	rules transfer.SelectionRules,
	observation getObservation,
	overrides ...getTransferDependencies,
) (*transfer.TransferJob, getOutputOperation, string, bool, int) {
	selection, err := transfer.NewSelectionSpec(
		runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.commandFailure(ExitFailure, err)
	}
	var dependencies getTransferDependencies = runtime
	if len(overrides) != 0 {
		dependencies = overrides[0]
	}
	admission, err := resolveGetOutputOperation(ctx, output.authority, dependencies, selection)
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
	job, err := dependencies.NewTransferJob(
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
		if reservation.PhysicalName() == "" {
			return "", false, errGetOutputReservationContract
		}
		return filepath.Join(displayRoot, reservation.PhysicalName()), reservation.CollisionIndex() > 0, nil
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
		return observation.commandFailureCode(ExitFailure, clievent.FailureOutputNeedsAttention)
	case errors.Is(err, errGetOutputOperationAmbiguous):
		return observation.commandFailureCode(ExitFailure, clievent.FailureOutputOwnership)
	case errors.Is(err, errGetOutputReservationContract), errors.Is(err, errGetOutputAdapterContract):
		return observation.commandFailureCode(ExitFailure, clievent.FailureOutputContract)
	default:
		return observation.commandFailure(ExitFailure, err)
	}
}
func receiverRoutePolicy(policy ConnectivityPolicy) transfer.ContentRoutePolicy {
	switch policy {
	case ConnectivityP2POnly:
		return transfer.ContentRouteDirectOnly
	case ConnectivityRelayOnly:
		return transfer.ContentRouteRelayOnly
	default:
		return transfer.ContentRouteAll
	}
}

func (a *App) recoverGetReceiver(ctx context.Context, request getRequest, observation getObservation) (*getReceiverSession, error) {
	lifetime, cancel := context.WithTimeout(ctx, getSessionRecoveryWindow)
	defer cancel()
	for {
		session, err := a.openGetReceiver(ctx, request.link, request.connectivity, observation, lifetime)
		if err == nil {
			return session, nil
		}
		var joinFailure *relayset.ReceiverJoinFailure
		if !errors.As(err, &joinFailure) {
			return nil, err
		}
		request.link.Relays = joinFailure.RetryEndpoints()
		if len(request.link.Relays) == 0 {
			return nil, err
		}
		timer := time.NewTimer(getRelayStartingRetryDelay)
		select {
		case <-lifetime.Done():
			timer.Stop()
			return nil, errors.Join(err, lifetime.Err())
		case <-timer.C:
		}
	}
}
