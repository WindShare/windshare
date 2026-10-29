package receive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/liveshare"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/transport/relayv2"
)

var (
	errGetOutputOperationAlreadyRunning = errors.New("get output operation is already running")
	errGetOutputOperationNeedsAttention = errors.New("get output operation needs attention")
	errGetOutputOperationAmbiguous      = errors.New("get output operation ownership is ambiguous")
	ErrRetainedOutputMissing            = errors.New("no retained download matches this link and selection")
	errGetOutputReservationContract     = errors.New("get output operation reservation violated its contract")
)

type getOutputPreparation struct {
	admission    OutputAdmission
	operationID  receivecontract.OperationID
	operation    *getOutputOperation
	contentReady chan struct{}
	paths        *receiverContentPaths
	authority    getOutputAuthority
	mode         getOutputMode
	displayRoot  string
}

func (a *runner) prepareGetOutput(
	ctx context.Context,
	request getRequest,
	observation getObservation,
) (getOutputPreparation, stepOutcome) {
	outputRoot := request.outDir
	// Starting the command certifies the caller-provided container. The operation
	// identity is resolved later, after selection is frozen, so a repeated command
	// can reopen exactly one compatible owned reservation.
	factory := a.getOutputFactory
	if factory == nil {
		return getOutputPreparation{}, observation.fail(stepInvalidRequest, ErrMissingOutputFactory)
	}
	authority, err := factory.NewOutputAuthority(getOutputAuthorityConfig{

		Diagnostics: observation.control.Diagnostics,
	})
	if err != nil {
		if authority != nil {
			observation.recordCleanup(authority.Close())
		}
		return getOutputPreparation{}, observation.fail(stepLocalFailure, errors.Join(err, observation.cleanupFailure()))
	}
	if authority == nil {
		return getOutputPreparation{}, observation.fail(stepLocalFailure, errGetOutputAdapterContract)
	}
	mode, err := authority.BindDestination(ctx)
	if err != nil {
		closeErr := authority.Close()
		observation.recordCleanup(closeErr)
		return getOutputPreparation{}, observation.fail(stepLocalFailure, errors.Join(err, closeErr))
	}
	if !mode.Valid() {
		closeErr := authority.Close()
		observation.recordCleanup(closeErr)
		return getOutputPreparation{}, observation.fail(stepLocalFailure, errors.Join(errGetOutputAdapterContract, closeErr))
	}
	return getOutputPreparation{
		authority: authority, mode: mode, displayRoot: outputRoot, operation: new(getOutputOperation), admission: request.admission, operationID: request.operationID,
		contentReady: make(chan struct{}), paths: newReceiverContentPaths(observation),
	}, stepReady
}

type getReceiverRecovery struct {
	owner       *relayset.ReceiverRecovery
	config      relayset.ReceiverConfig
	observation getObservation
}

func (a *runner) connectGetReceiver(ctx context.Context, request getRequest, observation getObservation) (*getReceiverSession, stepOutcome) {
	options := a.receiverRecoveryOptions
	options.WaitTimeout = request.waitTimeout
	options.Observe = observation.receiverRecovery
	owner, err := relayset.NewReceiverRecovery(options)
	if err != nil {
		return nil, observation.fail(stepLocalFailure, err)
	}
	recovery := &getReceiverRecovery{
		owner:       owner,
		observation: observation,
		config: relayset.ReceiverConfig{
			Dial: a.receiverDial,
			Receiver: liveshare.ReceiverConfig{
				Capacity:   a.receiveCapacity,
				Capability: request.link, ContentRoutePolicy: receiverRoutePolicy(request.connectivity),
				PeerControls: v2signal.ReceiverControlValidator{}, ProtocolObservations: observation.protocolObservations(),
				LaneSettlementObservationCapacity: observation.laneSettlementObservationCapacity(),
			},
			DialOptions: relayv2.DialOptions{LifecycleObservationCapacity: observation.relayObservationCapacity()},
			Connected: func(connection relayset.ReceiverConnection) {
				if source, ok := connection.(relayObservationSource); ok {
					observation.registerRelayConnection(source)
				}
				observation.relayConnected(connection.Endpoint())
			},
		},
	}
	session, err := recovery.open(ctx, owner.Join)
	if err != nil {
		return nil, observation.fail(stepNetworkFailure, err)
	}
	return session, stepReady
}

func (recovery *getReceiverRecovery) replace(ctx context.Context) (*getReceiverSession, error) {
	return recovery.open(ctx, recovery.owner.Replace)
}

func (recovery *getReceiverRecovery) open(ctx context.Context, connect func(context.Context, relayset.ReceiverConfig) (*relayset.Receiver, error)) (*getReceiverSession, error) {
	set, err := connect(ctx, recovery.config)
	if err != nil {
		return nil, err
	}
	runtime, connection, err := set.WaitReady(ctx)
	if err != nil {
		set.Close()
		return nil, err
	}
	recovery.observation.registerLaneSet(runtime.LaneSet())
	return &getReceiverSession{relays: set, connection: connection, runtime: runtime, recovery: recovery}, nil
}

type getReceiverSession struct {
	recovery   *getReceiverRecovery
	relays     *relayset.Receiver
	connection relayset.ReceiverConnection
	prepared   *liveshare.PreparedReceiver
	runtime    *receiversession.Runtime
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

type getTransferExecution struct {
	contentReady chan struct{}
	paths        *receiverContentPaths
	runtime      *receiversession.Runtime
	admission    receiverContentAdmission
	monitorDone  <-chan struct{}
	peer         *activeReceiverPeer
	localStop    *receiverLocalStop
	closeOnce    sync.Once
	settleOnce   sync.Once
}

// The immutable transfer plan belongs to the operation, while each execution
// owns only one protocol generation. Recovery never copies mutable job state.
type getTransferPlan struct {
	operation           getOutputOperation
	destination         string
	destinationAdjusted bool
	job                 *transfer.TransferJob
}

func (execution *getTransferExecution) Close() {
	execution.CloseWithReason(ReceiverLocalStopCaller)
}

func (execution *getTransferExecution) CloseWithReason(reason ReceiverLocalStopReason) {
	if execution == nil {
		return
	}
	execution.closeOnce.Do(func() {
		if execution.peer != nil {
			execution.peer.CloseWithReason(reason)
		}
		execution.SettleAdmission()
	})
}

func (execution *getTransferExecution) SettleAdmission() {
	if execution == nil {
		return
	}
	execution.settleOnce.Do(func() {
		execution.admission.Close()
		execution.admission.Wait()
		if execution.monitorDone != nil {
			<-execution.monitorDone
		}
	})
}

func (a *runner) prepareGetConnectivity(ctx context.Context, request getRequest, output getOutputPreparation, runtime *receiversession.Runtime, observation getObservation, options receiverPeerOptions) (*getTransferExecution, stepOutcome) {
	connectivity, err := request.connectivity.receiverPlan()
	if err != nil {
		return nil, observation.failCode(stepInvalidRequest, FailureInvalidInput)
	}
	laneID, laneEpoch := runtime.LaneIdentity()
	relaySuspension, err := runtime.LaneSet().SuspendContent(
		transferlanes.Identity{ID: laneID, Epoch: laneEpoch},
	)
	if err != nil {
		return nil, observation.fail(stepLocalFailure, err)
	}
	contentReady := output.contentReady
	if contentReady == nil {
		contentReady = make(chan struct{})
	}
	paths := output.paths
	if paths == nil {
		paths = newReceiverContentPaths(observation)
	}
	observation.session = runtime.ProtocolSessionID()
	observation.paths = paths
	admission, err := newReceiverContentAdmissionWithExecution(
		connectivity.relayContent,
		relaySuspension,
		receiverAdmissionExecution{
			claimGate: contentReady,
			onClaim:   observation.relayContentClaimed,
		},
	)
	if err != nil {
		return nil, observation.fail(stepLocalFailure, err)
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
			localStop.record(ReceiverLocalStopOutputAdmission)
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
			observation.failCode(stepNetworkFailure, FailurePeerNegotiation)
			execution.CloseWithReason(ReceiverLocalStopRuntimeSessionFailure)
			return nil, stepNetworkFailure
		}
		observation.fail(stepInvalidRequest, err)
		execution.CloseWithReason(ReceiverLocalStopCaller)
		return nil, stepInvalidRequest
	}
	return execution, stepReady
}

type getTransferDependencies interface {
	getShapeResolver
	NewTransferJob(transfer.ReceiveIntent, transfer.TransferJobID, transfer.DirectTreeMaterializer, transfer.TransferLifecycleTracer) (*transfer.TransferJob, error)
}

func (a *runner) finishGetTransfer(ctx context.Context, request getRequest, output getOutputPreparation, dependencies getTransferDependencies, execution *getTransferExecution, observation getObservation) (*getTransferPlan, stepOutcome) {
	rules, err := selectionRules(request.only)
	if err != nil {
		return nil, observation.fail(stepInvalidRequest, err)
	}
	job, operation, destination, adjusted, code := a.buildGetTransferJob(
		ctx, execution.runtime, output, rules, observation, dependencies,
	)
	if code != stepReady {
		if errors.Is(execution.admission.Err(), errReceiverP2PPathUnavailable) {
			return nil, stepNetworkFailure
		}
		return nil, code
	}
	if output.mode == getOutputLiveOnly {
		observation.warningCode(FailureOutputRecoveryUnavailable)
	}
	// Lane timing may queue relay admission while shape and destination authority
	// are being resolved. Releasing this separate gate only after the immutable
	// operation and job exist prevents any content request from outrunning them.
	if observation.metrics != nil {
		observation.metrics.Activate(fmt.Sprintf("%x", job.JobID().Bytes()))
	}
	execution.paths.setTransfer(job.ReceiveIntent().OperationID(), job.JobID())
	close(execution.contentReady)
	return &getTransferPlan{operation: operation, destination: destination, destinationAdjusted: adjusted, job: job}, stepReady
}

func (a *runner) buildGetTransferJob(
	ctx context.Context,
	runtime *receiversession.Runtime,
	output getOutputPreparation,
	rules transfer.SelectionRules,
	observation getObservation,
	overrides ...getTransferDependencies,
) (*transfer.TransferJob, getOutputOperation, string, bool, stepOutcome) {
	selection, err := transfer.NewSelectionSpec(
		runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.fail(stepLocalFailure, err)
	}
	var dependencies getTransferDependencies = runtime
	if len(overrides) != 0 {
		dependencies = overrides[0]
	}
	var admission getOutputAdmission
	if output.operation != nil && output.operation.Valid() {
		admission.operation = *output.operation
		if admission.operation.Intent.SelectionSpec().Digest() != selection.Digest() {
			err = transfer.ErrInvalidOutputBinding
		}
	} else {
		admission, err = resolveGetOutputOperation(ctx, output.authority, dependencies, selection, output.admission, output.operationID)
		if err == nil && output.operation != nil {
			*output.operation = admission.operation
		}
	}
	if err != nil {
		return nil, getOutputOperation{}, "", false, reportGetOutputAdmissionFailure(observation, err)
	}
	if admission.operation.Mode != output.mode {
		return nil, getOutputOperation{}, "", false,
			reportGetOutputAdmissionFailure(observation, errGetOutputReservationContract)
	}
	destination, adjusted, err := getOperationDestination(output.displayRoot, admission.operation)
	if err != nil {
		return nil, getOutputOperation{}, "", false, reportGetOutputAdmissionFailure(observation, err)
	}
	var identity [receivecontract.StableIdentityBytes]byte
	_, err = io.ReadFull(a.control.Random, identity[:])
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.fail(stepLocalFailure, err)
	}
	jobID, err := transfer.TransferJobIDFromBytes(identity[:])
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.fail(stepLocalFailure, err)
	}
	job, err := dependencies.NewTransferJob(
		admission.operation.Intent,
		jobID,
		getOperationMaterializer{operation: admission.operation},
		transfer.TransferLifecycleTraceFunc(observation.transferLifecycle),
	)
	if err != nil {
		return nil, getOutputOperation{}, "", false, observation.fail(stepLocalFailure, err)
	}
	return job, admission.operation, destination, adjusted, stepReady
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

func getOperationDestination(displayRoot string, operation getOutputOperation) (string, bool, error) {
	if !operation.Valid() {
		return "", false, errGetOutputReservationContract
	}
	if operation.Destination != "" {
		return operation.Destination, operation.DestinationAdjusted, nil
	}
	return displayRoot, operation.DestinationAdjusted, nil
}

func resolveGetOutputOperation(
	ctx context.Context,
	authority getOutputAuthority,
	resolver getShapeResolver,
	selection transfer.SelectionSpec,
	policy OutputAdmission,
	operationID receivecontract.OperationID,
) (getOutputAdmission, error) {
	if ctx == nil || authority == nil || resolver == nil || selection.IsZero() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	selector := receivecontract.AutomaticOperation()
	if policy == OutputRedownload {
		selector = receivecontract.RedownloadOperation()
	}
	if !operationID.IsZero() {
		if policy != OutputResumeExisting {
			return getOutputAdmission{}, errGetOutputReservationContract
		}
		selector, _ = receivecontract.SelectOperationID(operationID)
	}
	lookup, err := authority.SelectOperation(ctx, selection, selector)
	if err != nil {
		return getOutputAdmission{}, err
	}
	if !lookup.Valid() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	var operation getOutputOperation
	switch lookup.Kind {
	case getOutputLookupNotFound:
		if selector.Kind() != receivecontract.OperationByID {
			return getOutputAdmission{}, errGetOutputReservationContract
		}
		return getOutputAdmission{}, ErrRetainedOutputMissing
	case getOutputLookupCreateReady:
		if policy == OutputResumeExisting {
			return getOutputAdmission{}, ErrRetainedOutputMissing
		}
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
		operation, err = lookup.Reservation.Create(ctx, artifact)
		if err != nil {
			return getOutputAdmission{}, err
		}
	case getOutputLookupReopened:
		operation = lookup.Operation
	case getOutputLookupAlreadyRunning:
		return getOutputAdmission{}, errGetOutputOperationAlreadyRunning
	case getOutputLookupNeedsAttention:
		return getOutputAdmission{}, errors.Join(errGetOutputOperationNeedsAttention, &output.AttentionError{Reason: lookup.Attention})
	case getOutputLookupAmbiguous:
		return getOutputAdmission{}, errGetOutputOperationAmbiguous
	default:
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	if !operation.Valid() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	reservation, direct := operation.Intent.MaterializationPlan().DestinationReservation()
	if !direct || reservation.IsZero() {
		return getOutputAdmission{}, errGetOutputReservationContract
	}
	return getOutputAdmission{
		operation: operation, lookup: lookup.Kind,
	}, nil
}

func reportGetOutputAdmissionFailure(observation getObservation, err error) stepOutcome {
	observation.fail(stepLocalFailure, err)
	switch {
	case errors.Is(err, ErrRetainedOutputMissing):
		return observation.failCode(stepLocalFailure, FailureRetainedOutputMissing)
	case errors.Is(err, errGetOutputOperationAlreadyRunning):
		return observation.failCode(stepLocalFailure, FailureOutputFileAlreadyActive)
	case errors.Is(err, errGetOutputOperationNeedsAttention):
		return observation.failCode(stepLocalFailure, FailureOutputNeedsAttention)
	case errors.Is(err, errGetOutputOperationAmbiguous):
		return observation.failCode(stepLocalFailure, FailureOutputOwnership)
	case errors.Is(err, errGetOutputReservationContract), errors.Is(err, errGetOutputAdapterContract):
		return observation.failCode(stepLocalFailure, FailureOutputContract)
	default:
		return observation.fail(stepLocalFailure, err)
	}
}
func receiverRoutePolicy(policy ConnectivityPolicy) transferlanes.ContentRoutePolicy {
	switch policy {
	case ConnectivityP2POnly:
		return transferlanes.ContentRouteDirectOnly
	case ConnectivityRelayOnly:
		return transferlanes.ContentRouteRelayOnly
	default:
		return transferlanes.ContentRouteAll
	}
}
