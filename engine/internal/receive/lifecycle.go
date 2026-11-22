package receive

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session/receivercontinuation"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
)

// receiveOperation retains output authority and metrics while replacing its
// synchronized session/admission pair. Shutdown joins replacements before taking
// the final ownership snapshot so no generation can outlive task completion.
type receiveOperation struct {
	runner       *runner
	input        preparedRequest
	output       getOutputPreparation
	observation  getObservation
	metrics      *downloadmetrics.Metrics
	options      receiverPeerOptions
	continuation *receivercontinuation.Session

	mu        sync.Mutex
	session   *getReceiverSession
	execution *getTransferExecution
}

func (g *receiveOperation) close(resolveStop func(*getReceiverSession, *getTransferExecution) ReceiverLocalStopReason) error {
	if g.continuation != nil {
		g.continuation.Close()
	}
	g.mu.Lock()
	session, execution := g.session, g.execution
	g.mu.Unlock()
	stopReason := ReceiverLocalStopCaller
	if resolveStop != nil {
		stopReason = resolveStop(session, execution)
	}
	execution.CloseWithReason(stopReason)
	session.Close()
	cleanupErr := g.observation.cleanupFailure()
	if session != nil && session.runtime != nil {
		cleanupErr = errors.Join(cleanupErr, session.runtime.CleanupError())
	}
	if g.options.native != nil {
		cleanupCtx, cancel := g.runner.control.CleanupContext()
		cleanupErr = errors.Join(cleanupErr, g.options.native.Close(cleanupCtx))
		cancel()
	}
	g.observation.complete()
	return cleanupErr
}

func (g *receiveOperation) replace(ctx context.Context, _ *receiversession.Runtime) (receivercontinuation.Replacement, error) {
	g.mu.Lock()
	oldSession, oldExecution := g.session, g.execution
	g.mu.Unlock()
	oldExecution.CloseWithReason(ReceiverLocalStopRuntimeSessionFailure)
	oldSession.Close()
	if err := oldSession.runtime.CleanupError(); err != nil {
		g.observation.recordCleanup(err)
		return receivercontinuation.Replacement{}, err
	}
	g.options.native.CloseSession([16]byte(oldSession.runtime.ProtocolSessionID()))
	g.observation.completeGeneration()
	next, err := oldSession.recovery.replace(ctx)
	if err != nil {
		return receivercontinuation.Replacement{}, err
	}
	next.runtime.BindDownloadMetrics(g.metrics)
	nextExecution, step := g.runner.prepareGetConnectivity(next.ctx, g.input, g.output, next.runtime, g.observation, g.options)
	if step != stepReady {
		next.Close()
		failure := g.observation.failureSnapshot()
		return receivercontinuation.Replacement{}, errors.Join(errors.New("replacement content admission failed"), failure.Cause)
	}
	return receivercontinuation.Replacement{
		Runtime: next.runtime,
		Commit: func() {
			g.mu.Lock()
			g.session, g.execution = next, nextExecution
			g.mu.Unlock()
			generation := GenerationChanged{Previous: oldSession.runtime.ProtocolSessionID(), Current: next.runtime.ProtocolSessionID()}
			generation.Operation, generation.Job = g.output.paths.transferIdentity()
			g.observation.emit(generation)
		},
		Discard: func(err error) {
			reason := ReceiverLocalStopRuntimeSessionFailure
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				reason = ReceiverLocalStopCaller
			}
			nextExecution.CloseWithReason(reason)
			next.Close()
		},
	}, nil
}

func (g *receiveOperation) demandContent() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.execution.peer.SetDemand(peerset.ContentDemand)
}

func (g *receiveOperation) observeContent(_ transfer.ReceiveProgressSnapshot) {
	g.mu.Lock()
	current := g.execution
	g.mu.Unlock()
	current.paths.observeContent(current.runtime.LaneSet().ContentActivity(), g.runner.clock.Now())
}
