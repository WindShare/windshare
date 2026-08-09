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
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/transport/relayv2"
)

var (
	errGetOutputOperationNeedsAttention = errors.New("get output operation needs attention")
	errGetOutputReservationContract     = errors.New("get output operation reservation violated its contract")
)

type getDirectTreeReservationAuthority interface {
	ReserveDirectTree(
		context.Context,
		transfer.SelectionSpec,
		receivecontract.ArtifactSpec,
	) (osfs.NativeDirectTreeReservation, error)
}

type getOutputPreparation struct {
	authority *osfs.FilesystemOutputAuthority
	clock     receiverAdmissionClock
	startedAt time.Time
}

func (a *App) prepareGetOutput(request getRequest) (getOutputPreparation, int) {
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
	authority, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: outputRoot, CreateRoot: true,
		Tracer: osfs.FilesystemOutputTraceFunc(a.traceFilesystemOutput),
	})
	if err != nil {
		a.logf("get: initialize output authority: %v", err)
		return getOutputPreparation{}, ExitFailure
	}
	return getOutputPreparation{
		authority: authority, clock: clock, startedAt: startedAt,
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
	admission, err := newRelayContentAdmission(output.startedAt, output.clock, relaySuspension)
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
	job, code := a.buildGetTransferJob(ctx, runtime, output, rules)
	if code != ExitOK {
		execution.Close()
		return nil, code
	}
	execution.job = job
	return execution, ExitOK
}

func (a *App) buildGetTransferJob(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	output getOutputPreparation,
	rules transfer.SelectionRules,
) (*transfer.TransferJob, int) {
	selection, err := transfer.NewSelectionSpec(
		runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		a.logf("get: freeze selection: %v", err)
		return nil, ExitFailure
	}
	intent, _, err := reserveGetOutputOperation(ctx, output.authority, selection)
	if err != nil {
		if errors.Is(err, errGetOutputOperationNeedsAttention) {
			a.logf("get: output operation needs attention because ownership is ambiguous")
			return nil, ExitFailure
		}
		if errors.Is(err, errGetOutputReservationContract) {
			a.logf("get: output operation reservation violated its contract")
			return nil, ExitFailure
		}
		a.logf("get: reserve output operation: %v", err)
		return nil, ExitFailure
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		a.logf("get: allocate transfer job identity: %v", err)
		return nil, ExitFailure
	}
	job, err := runtime.NewTransferJob(
		intent, jobID, output.authority,
		transfer.TransferLifecycleTraceFunc(a.traceTransferLifecycle),
	)
	if err != nil {
		a.logf("get: initialize transfer: %v", err)
		return nil, ExitFailure
	}
	return job, ExitOK
}

func reserveGetOutputOperation(
	ctx context.Context,
	authority getDirectTreeReservationAuthority,
	selection transfer.SelectionSpec,
) (transfer.ReceiveIntent, osfs.NativeDirectTreeReservationKind, error) {
	if authority == nil || selection.IsZero() {
		return transfer.ReceiveIntent{}, 0, errGetOutputReservationContract
	}
	// Catalog-root DirectoryTree is the CLI's existing source-path-under-`-o`
	// layout. Freezing it here keeps repeat-command lookup independent from any
	// browser task-root or artifact-choice semantics.
	reservation, err := authority.ReserveDirectTree(
		ctx, selection, receivecontract.NewCatalogRootDirectoryTree(),
	)
	if err != nil {
		return transfer.ReceiveIntent{}, 0, err
	}
	kind := reservation.Kind()
	if kind == osfs.NativeDirectTreeNeedsAttention {
		return transfer.ReceiveIntent{}, kind, errGetOutputOperationNeedsAttention
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok || (kind != osfs.NativeDirectTreeReserved && kind != osfs.NativeDirectTreeReopened) {
		return transfer.ReceiveIntent{}, kind, errGetOutputReservationContract
	}
	return intent, kind, nil
}
