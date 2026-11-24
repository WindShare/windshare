package receive

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/receivercontinuation"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine/internal/task"
)

const (
	getProgressInterval       = 500 * time.Millisecond
	receiveSleepInhibitReason = "WindShare active download"
)

func runAttempt(ctx context.Context, input preparedRequest, deps Dependencies, retained *getOutputPreparation) (completed task.Completion[Result]) {
	a := &runner{receiverRecoveryOptions: deps.Recovery, receiverPeerFactory: deps.PeerFactory, receiverDial: deps.ReceiverDial, getOutputFactory: input.output, control: deps.Control, clock: deps.Clock}
	a.receiveCapacity = deps.Capacity
	a.connectivityCapacity = deps.Connectivity
	a.resolveEndpoints = deps.ResolveEndpoints
	startedAt := a.clock.Now()
	metrics := downloadmetrics.Prepare(a.clock.Now)
	observation := newObservation(deps.Control, deps.Control.Diagnostics.Enabled(), metrics)
	active := &receiveOperation{runner: a, input: input, observation: observation, metrics: metrics}
	var settlement *SettlementInput
	step := stepReady
	defer func() { completed = active.settleAttempt(ctx, settlement, step, startedAt) }()
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
	nativeConfig := nativepeer.Config{Side: nativepeer.SideReceiver, Now: a.clock.Now, Pool: deps.ICEPool}
	if deps.Control.Diagnostics.Enabled() {
		nativeConfig.ObservationCapacity = nativepeer.DefaultObservationCapacity
	}
	native, err := deps.Connectivity.NewNative(nativeConfig)
	if err != nil {
		step = observation.fail(stepLocalFailure, err)
		return
	}
	active.options = receiverPeerOptions{budget: peerset.NewBudget(startedAt), native: native}
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
	result := runWithSleepInhibition(deps.SleepInhibitor, func() transfer.JobResult {
		return a.runTransferJob(ctx, prepared.job, observation, active.observeContent)
	})
	active.continuation.Close()
	admissionErr := active.execution.admission.Err()
	runtimeErr, connectionErr := receiverTerminationErrors(active.session.runtime, active.session.connection)
	if !active.session.runtime.PathsExhausted() {
		connectionErr = nil
	}
	active.execution.CloseWithReason(getSettlementStopReason(ctx.Err(), admissionErr, runtimeErr, connectionErr))
	var nameAdjustments NameAdjustmentSummary
	if prepared.operation.Control != nil {
		nameAdjustments = prepared.operation.Control.NameAdjustments()
	}
	settlement = &SettlementInput{Result: result, AdmissionError: admissionErr, RuntimeError: runtimeErr, ConnectionError: connectionErr, ContextError: ctx.Err(), Destination: prepared.destination, DestinationAdjusted: prepared.destinationAdjusted, NameAdjustments: nameAdjustments, Operation: prepared.job.ReceiveIntent().OperationID(), Job: prepared.job.JobID()}
	return
}

// Teardown participates in settlement, so recovery advice must include its
// outcome rather than being decided while an attempt still owns resources.
func (active *receiveOperation) settleAttempt(ctx context.Context, settlement *SettlementInput, step stepOutcome, startedAt time.Time) task.Completion[Result] {
	cleanupErr := active.close()
	elapsed := max(active.runner.clock.Now().Sub(startedAt), 0)
	connectivity := active.metrics.Snapshot(true)
	var peerStop, relayStop error
	if active.session != nil {
		if err := active.session.runtime.Err(); session.PeerStopped(err) {
			peerStop = err
		}
		if active.session.relays.AllRelaysStopped() {
			relayStop = relayset.ErrReceiverShareStopped
		}
	}
	if settlement != nil {
		if peerStop != nil {
			settlement.RuntimeError = peerStop
		}
		if relayStop != nil {
			settlement.ConnectionError = errors.Join(settlement.ConnectionError, relayStop)
		}
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
	failure := active.observation.failureSnapshot()
	failure.Cause = errors.Join(failure.Cause, peerStop, relayStop)
	completed := settlePreparationFailure(step, failure, ctx.Err(), cleanupErr)
	completed.Value.Destination = active.input.outDir
	completed.Value.Elapsed = elapsed
	completed.Value.Connectivity = connectivity
	return completed
}

func runWithSleepInhibition(inhibitor SleepInhibitor, fn func() transfer.JobResult) transfer.JobResult {
	if inhibitor != nil {
		if release, err := inhibitor.InhibitSleep(receiveSleepInhibitReason); err == nil && release != nil {
			defer release()
		}
	}
	return fn()
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
