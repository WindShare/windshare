package receive

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/receivercontinuation"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine/internal/task"
)

const getProgressInterval = 500 * time.Millisecond

func Run(ctx context.Context, request Request, deps Dependencies) (completed task.Completion[Result]) {
	deps = deps.normalized()
	deps.Control.Diagnostics = request.Diagnostics
	a := &runner{receiverRecoveryOptions: deps.Recovery, receiverPeerFactory: deps.PeerFactory, receiverDial: deps.ReceiverDial, getOutputFactory: request.Output, control: deps.Control, clock: deps.Clock}
	a.receiveCapacity = deps.Capacity
	a.connectivityCapacity = deps.Connectivity
	input := getRequest{outDir: request.Destination, only: append([]string(nil), request.Only...), link: request.Capability, connectivity: request.Connectivity, waitTimeout: request.WaitTimeout}
	startedAt := a.clock.Now()
	metrics := downloadmetrics.Prepare(a.clock.Now)
	observation := newObservation(deps.Control, deps.Control.Diagnostics.Enabled(), metrics)
	active := &receiveOperation{runner: a, input: input, observation: observation, metrics: metrics}
	var settlement *SettlementInput
	step := stepReady
	defer func() {
		cleanupErr := active.close()
		completed.CleanupError = cleanupErr
		if settlement != nil {
			settlement.CleanupError = cleanupErr
			settlement.Elapsed = max(a.clock.Now().Sub(startedAt), 0)
			settlement.Connectivity = metrics.Snapshot(true)
			value, err := Settle(*settlement)
			if err != nil {
				completed.Settlement = task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal,
					Err: errors.Join(Failure{Cause: err}, settlement.diagnosticCauses()), CleanupError: cleanupErr}
			} else {
				completed = value
			}
			return
		}
		completed = settlePreparationFailure(step, observation.failureSnapshot(), ctx.Err(), cleanupErr)
		completed.Value.Elapsed = max(a.clock.Now().Sub(startedAt), 0)
		completed.Value.Connectivity = metrics.Snapshot(true)
	}()
	if request.Capability.Suite != link.SuiteSenderAuthenticated || request.WaitTimeout < 0 {
		step = observation.failCode(stepInvalidRequest, FailureInvalidInput)
		return
	}
	if _, err := request.Connectivity.receiverPlan(); err != nil {
		step = observation.fail(stepInvalidRequest, err)
		return
	}
	active.output, step = a.prepareGetOutput(ctx, input, observation)
	if step != stepReady {
		return
	}
	active.session, step = a.connectGetReceiver(ctx, input, observation)
	if step != stepReady {
		return
	}
	observation.emit(GenerationChanged{Current: active.session.runtime.ProtocolSessionID()})
	nativeConfig := nativepeer.Config{Side: nativepeer.SideReceiver, Now: a.clock.Now}
	if deps.Control.Diagnostics.Enabled() {
		nativeConfig.ObservationCapacity = nativepeer.DefaultObservationCapacity
	}
	native, err := deps.Connectivity.NewNative(nativeConfig)
	if err != nil {
		step = observation.fail(stepLocalFailure, err)
		return
	}
	active.options = receiverPeerOptions{budget: peerset.NewBudget(startedAt), capacity: deps.Connectivity.Attempts(), native: native}
	observation.registerNative(active.options.native)
	active.session.runtime.BindDownloadMetrics(metrics)
	active.execution, step = a.prepareGetConnectivity(ctx, input, active.output, active.session.runtime, observation, active.options)
	if step != stepReady {
		return
	}
	active.continuation, err = receivercontinuation.New(ctx, active.session.runtime, active.replace)
	if err != nil {
		step = observation.fail(stepLocalFailure, err)
		return
	}
	active.continuation.BindDownloadMetrics(metrics)
	prepared, prepareStep := a.finishGetTransfer(ctx, input, active.output, active.continuation, active.execution, observation)
	step = prepareStep
	if step != stepReady {
		return
	}
	active.demandContent()
	result := a.runTransferJob(ctx, prepared.job, observation, active.observeContent)
	active.continuation.Close()
	admissionErr := active.execution.admission.Err()
	runtimeErr, connectionErr := receiverTerminationErrors(active.session.runtime, active.session.connection)
	if !active.session.runtime.PathsExhausted() {
		connectionErr = nil
	}
	active.execution.CloseWithReason(getSettlementStopReason(ctx.Err(), admissionErr, runtimeErr, connectionErr))
	settlement = &SettlementInput{Result: result, AdmissionError: admissionErr, RuntimeError: runtimeErr, ConnectionError: connectionErr, ContextError: ctx.Err(), Destination: prepared.destination, DestinationAdjusted: prepared.destinationAdjusted, Operation: prepared.job.ReceiveIntent().OperationID(), Job: prepared.job.JobID()}
	return
}

func selectionRules(requested []string) (transfer.SelectionRules, error) {
	if len(requested) == 0 {
		return transfer.NewSelectionRules(true, nil)
	}
	return transfer.NewPathSelectionRules(requested)
}
func (a *runner) runTransferJob(ctx context.Context, job *transfer.TransferJob, observation getObservation, observeSelection func(transfer.ReceiveProgressSnapshot)) transfer.JobResult {
	snapshots := job.ProgressSnapshots()
	result := make(chan transfer.JobResult, 1)
	go func() { result <- job.Run(ctx) }()
	operation, jobID := job.ReceiveIntent().OperationID(), job.JobID()
	observation.progress(operation, jobID, job.Progress())
	ticker := a.clock.NewTicker(getProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case completed := <-result:
			if snapshots != nil {
				for snapshot := range snapshots {
					if observeSelection != nil {
						observeSelection(snapshot)
					}
				}
			}
			observation.progress(operation, jobID, completed.Progress)
			return completed
		case snapshot, ok := <-snapshots:
			if !ok {
				snapshots = nil
				continue
			}
			if observeSelection != nil {
				observeSelection(snapshot)
			}
		case <-ticker.C():
			observation.progress(operation, jobID, job.Progress())
		}
	}
}
func receiverTerminationErrors(runtime *receiversession.Runtime, connection relayset.ReceiverConnection) (runtimeErr, connectionErr error) {
	if runtime != nil {
		runtimeErr = runtime.Err()
	}
	if connection != nil {
		connectionErr = connection.Err()
	}
	return
}
func getSettlementStopReason(contextErr, admissionErr, runtimeErr, connectionErr error) ReceiverLocalStopReason {
	switch {
	case contextErr != nil:
		return ReceiverLocalStopCaller
	case admissionErr != nil:
		return ReceiverLocalStopOutputAdmission
	case runtimeErr != nil || connectionErr != nil:
		return ReceiverLocalStopRuntimeSessionFailure
	default:
		return ReceiverLocalStopNormalCompletion
	}
}
