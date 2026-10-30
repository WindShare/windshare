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

func runAttempt(ctx context.Context, request Request, deps Dependencies, retained *getOutputPreparation) (completed task.Completion[Result]) {
	deps = deps.normalized()
	deps.Control.Diagnostics = request.Diagnostics
	a := &runner{receiverRecoveryOptions: deps.Recovery, receiverPeerFactory: deps.PeerFactory, receiverDial: deps.ReceiverDial, getOutputFactory: request.Output, control: deps.Control, clock: deps.Clock}
	a.receiveCapacity = deps.Capacity
	a.connectivityCapacity = deps.Connectivity
	input := getRequest{admission: request.Admission, operationID: request.OperationID, outDir: request.Destination, only: append([]string(nil), request.Only...), link: request.Capability, connectivity: request.Connectivity, waitTimeout: request.WaitTimeout}
	startedAt := a.clock.Now()
	metrics := downloadmetrics.Prepare(a.clock.Now)
	observation := newObservation(deps.Control, deps.Control.Diagnostics.Enabled(), metrics)
	active := &receiveOperation{runner: a, input: input, observation: observation, metrics: metrics}
	var settlement *SettlementInput
	step := stepReady
	defer func() { completed = active.settleAttempt(ctx, settlement, step, startedAt) }()
	if request.Capability.Suite != link.SuiteSenderAuthenticated || request.WaitTimeout < 0 || request.Admission > OutputRedownload || !request.OperationID.IsZero() && request.Admission != OutputResumeExisting {
		step = observation.failCode(stepInvalidRequest, FailureInvalidInput)
		return
	}
	if _, err := request.Connectivity.receiverPlan(); err != nil {
		step = observation.fail(stepInvalidRequest, err)
		return
	}
	if retained.authority == nil {
		*retained, step = a.prepareGetOutput(ctx, input, observation)
		if step != stepReady {
			return
		}
	}
	active.output = *retained
	active.output.contentReady = make(chan struct{})
	active.output.paths = newReceiverContentPaths(observation)
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

// Teardown participates in settlement, so recovery advice must include its
// outcome rather than being decided while an attempt still owns resources.
func (active *receiveOperation) settleAttempt(ctx context.Context, settlement *SettlementInput, step stepOutcome, startedAt time.Time) task.Completion[Result] {
	cleanupErr := active.close()
	elapsed := max(active.runner.clock.Now().Sub(startedAt), 0)
	connectivity := active.metrics.Snapshot(true)
	if settlement != nil {
		settlement.CleanupError = cleanupErr
		settlement.Elapsed = elapsed
		settlement.Connectivity = connectivity
		completed, err := Settle(*settlement)
		if err != nil {
			return task.Completion[Result]{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal,
				Err: errors.Join(Failure{Cause: err}, settlement.diagnosticCauses()), CleanupError: cleanupErr}
		}
		return completed
	}
	completed := settlePreparationFailure(step, active.observation.failureSnapshot(), ctx.Err(), cleanupErr)
	completed.Value.Destination = active.input.outDir
	completed.Value.Elapsed = elapsed
	completed.Value.Connectivity = connectivity
	return completed
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
