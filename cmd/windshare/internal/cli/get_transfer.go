package cli

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/transport/relayv2"
)

var (
	errGetOutputOperationAlreadyRunning = errors.New("get output operation is already running")
	errGetOutputOperationNeedsAttention = errors.New("get output operation needs attention")
	errGetOutputOperationAmbiguous      = errors.New("get output operation ownership is ambiguous")
	errGetOutputReservationContract     = errors.New("get output operation reservation violated its contract")
)

type getOutputPreparation struct {
	authority getOutputAuthority
	mode      getOutputMode
	warnings  *warningOnce
	clock     receiverAdmissionClock
	startedAt time.Time
}

func (a *App) prepareGetOutput(
	ctx context.Context,
	request getRequest,
) (getOutputPreparation, int) {
	outputRoot, err := filepath.Abs(request.outDir)
	if err != nil {
		a.logf("get: resolve output directory: %v", err)
		return getOutputPreparation{}, ExitFailure
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
		tracer: osfs.FilesystemOutputTraceFunc(a.traceFilesystemOutput),
	})
	if err != nil {
		a.logf("get: initialize output authority: %v", err)
		return getOutputPreparation{}, ExitFailure
	}
	mode, err := authority.BindDestination(ctx)
	if err != nil {
		closeErr := authority.Close()
		a.logf("get: bind output container: %v", errors.Join(err, closeErr))
		return getOutputPreparation{}, ExitFailure
	}
	a.traceGet(GetTraceEvent{Stage: GetTraceDestinationBound, Mode: mode})
	return getOutputPreparation{
		authority: authority, mode: mode, warnings: a.getWarningReporter(),
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

func (a *App) connectGetReceiver(ctx context.Context, capability link.Link) (*getReceiverSession, int) {
	connection, code := a.dialV2Receiver(ctx, capability)
	if code != ExitOK {
		return nil, code
	}
	prepared, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
		Capability: capability, DescriptorObject: connection.Descriptor(),
		PeerControls: v2signal.ReceiverControlValidator{},
	})
	if err != nil {
		_ = connection.Close()
		a.logf("get: authenticate descriptor: %v\nCheck that the link and key belong to this share.", err)
		return nil, ExitUsage
	}
	runtime, err := prepared.Connect(ctx, connection.Channel())
	if err != nil {
		prepared.Close()
		_ = connection.Close()
		if ctx.Err() != nil {
			a.logf("get: interrupted during session handshake")
			return nil, ExitFailure
		}
		a.logf("get: establish authenticated session: %v", err)
		return nil, ExitNetwork
	}
	return &getReceiverSession{
		connection: connection, prepared: prepared, runtime: runtime,
	}, ExitOK
}

type getTransferExecution struct {
	app         *App
	runtime     *sessionruntime.ReceiverRuntime
	admission   *relayContentAdmission
	monitorDone <-chan struct{}
	peer        *activeReceiverPeer
	operation   getOutputOperation
	renamed     bool
	job         *transfer.TransferJob
	settled     bool
	closed      bool
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
	execution.app.logReceiverAdmissionTraces(
		execution.runtime.ProtocolSessionID().Bytes(), execution.admission,
	)
}

func (a *App) prepareGetTransfer(
	ctx context.Context,
	request getRequest,
	output getOutputPreparation,
	runtime *sessionruntime.ReceiverRuntime,
) (*getTransferExecution, int) {
	laneID, laneEpoch := runtime.LaneIdentity()
	relaySuspension, err := runtime.LaneSet().SuspendContent(
		transfer.LaneIdentity{ID: laneID, Epoch: laneEpoch},
	)
	if err != nil {
		a.logf("get: initialize content-path admission: %v", err)
		return nil, ExitFailure
	}
	contentReady := make(chan struct{})
	admission, err := newRelayContentAdmissionWithExecution(
		output.startedAt,
		output.clock,
		relaySuspension,
		receiverAdmissionExecution{claimGate: contentReady},
	)
	if err != nil {
		a.logf("get: initialize content-path admission: %v", err)
		return nil, ExitFailure
	}
	execution := &getTransferExecution{
		app: a, runtime: runtime, admission: admission,
		monitorDone: a.monitorReceiverAdmission(admission, runtime),
	}
	observePeer := func(signal receiverPeerSignal) {
		if observeErr := admission.ObservePeer(signal); observeErr != nil {
			a.logf("get: apply direct-peer admission signal failed cause_class=relay_resume")
			runtime.Close()
		}
	}
	peer, rules, err := beginReceiverPlanning(
		request.connectivity,
		func() *activeReceiverPeer { return a.startReceiverPeer(ctx, runtime, observePeer) },
		func() { observePeer(receiverPeerFailed) },
		func() (transfer.SelectionRules, error) { return selectionRules(request.only) },
	)
	execution.peer = peer
	if err != nil {
		a.logf("get: resolve selection: %v", err)
		execution.Close()
		return nil, ExitUsage
	}
	job, operation, renamed, code := a.buildGetTransferJob(ctx, runtime, output, rules)
	if code != ExitOK {
		execution.Close()
		return nil, code
	}
	if output.mode == getOutputLiveOnly {
		output.warnings.Warn(liveOnlyOutputWarning)
	}
	execution.operation = operation
	execution.renamed = renamed
	execution.job = job
	// Lane timing may queue relay admission while shape and destination authority
	// are being resolved. Releasing this separate gate only after the immutable
	// operation and job exist prevents any content request from outrunning them.
	close(contentReady)
	a.traceGet(GetTraceEvent{
		Stage: GetTraceContentAdmitted, ProtocolSessionID: runtime.ProtocolSessionID(),
		IntentDigest: operation.intent.Digest(), Mode: operation.mode, Renamed: renamed,
	})
	return execution, ExitOK
}

func (a *App) buildGetTransferJob(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	output getOutputPreparation,
	rules transfer.SelectionRules,
) (*transfer.TransferJob, getOutputOperation, bool, int) {
	selection, err := transfer.NewSelectionSpec(
		runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		a.logf("get: freeze selection: %v", err)
		return nil, getOutputOperation{}, false, ExitFailure
	}
	admission, err := resolveGetOutputOperation(ctx, output.authority, runtime, selection, a)
	if err != nil {
		a.reportGetOutputAdmissionFailure(err)
		return nil, getOutputOperation{}, false, ExitFailure
	}
	if admission.operation.mode != output.mode {
		a.reportGetOutputAdmissionFailure(errGetOutputReservationContract)
		return nil, getOutputOperation{}, false, ExitFailure
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		a.logf("get: allocate transfer job identity: %v", err)
		return nil, getOutputOperation{}, false, ExitFailure
	}
	job, err := runtime.NewTransferJob(
		admission.operation.intent,
		jobID,
		getOperationMaterializer{authority: output.authority, operation: admission.operation},
		transfer.TransferLifecycleTraceFunc(a.traceTransferLifecycle),
	)
	if err != nil {
		a.logf("get: initialize transfer: %v", err)
		return nil, getOutputOperation{}, false, ExitFailure
	}
	return job, admission.operation, admission.renamed, ExitOK
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
	renamed   bool
}

func resolveGetOutputOperation(
	ctx context.Context,
	authority getOutputAuthority,
	resolver getShapeResolver,
	selection transfer.SelectionSpec,
	app *App,
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
	if app != nil {
		app.traceGet(GetTraceEvent{
			Stage: GetTraceActiveLookup, SelectionDigest: selection.Digest(), Lookup: lookup.kind,
		})
	}
	var operation getOutputOperation
	switch lookup.kind {
	case getOutputLookupMiss:
		var shapeTracer ordinaryoutput.ShapeTracer
		if app != nil {
			shapeTracer = ordinaryoutput.ShapeTraceFunc(app.traceOrdinaryOutputShape)
		}
		decision, resolveErr := resolver.ResolveOrdinaryOutputShape(
			ctx, selection, ordinaryoutput.DefaultShapeProbeBudgetV1, shapeTracer,
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
	admission := getOutputAdmission{
		operation: operation, lookup: lookup.kind, renamed: reservation.CollisionIndex() > 0,
	}
	if app != nil {
		app.traceGet(GetTraceEvent{
			Stage: GetTraceOperationReady, SelectionDigest: selection.Digest(),
			IntentDigest: operation.intent.Digest(), Mode: operation.mode,
			Lookup: lookup.kind, Renamed: admission.renamed,
		})
	}
	return admission, nil
}

func (a *App) reportGetOutputAdmissionFailure(err error) {
	switch {
	case errors.Is(err, errGetOutputOperationAlreadyRunning):
		a.logf("get: this download is already running for the selected destination")
	case errors.Is(err, errGetOutputOperationNeedsAttention):
		a.logf("get: output operation needs attention; run 'windshare resume list -o <directory>'")
	case errors.Is(err, errGetOutputOperationAmbiguous):
		a.logf("get: output operation ownership is ambiguous; no destination was selected")
	case errors.Is(err, errGetOutputReservationContract), errors.Is(err, errGetOutputAdapterContract):
		a.logf("get: output operation reservation violated its contract")
	default:
		a.logf("get: prepare output operation: %v", err)
	}
}
