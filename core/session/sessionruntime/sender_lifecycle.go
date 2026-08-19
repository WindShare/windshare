package sessionruntime

import (
	"context"
	"errors"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationCancelHandler interface {
	HandleMessage(context.Context, protocolsession.Message) error
}

type cancelMux struct {
	catalog operationCancelHandler
	content operationCancelHandler
	peer    SenderPeerHandler
}

func (mux cancelMux) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	operationID, ok := message.OperationID()
	if !ok {
		return ErrRuntimeConfig
	}
	if _, err := contentflow.DecodeCancelReason(message.Body()); err != nil {
		return err
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok {
		return ErrOperationMissing
	}
	requestKind, ok := generation.RequestKind()
	if !ok {
		// A preemptive CANCEL has no work owner. Delayed handlers independently
		// suppress its now-tombstoned generation before starting service work.
		return nil
	}
	switch requestKind {
	case protocolsession.MessageListChildren:
		return mux.catalog.HandleMessage(ctx, message)
	case protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks:
		return mux.content.HandleMessage(ctx, message)
	case protocolsession.MessagePeerOffer:
		return mux.peer.Cancel(ctx, operationID)
	case protocolsession.MessageLaneAttach:
		return nil
	default:
		return ErrRuntimeConfig
	}
}

func registerSenderHandlers(
	router *protocolsession.RoleRouter,
	catalogHandler *catalogHandler,
	contentHandler *contentflow.SenderHandler,
	laneHandler *laneGrantHandler,
	peerHandler SenderPeerHandler,
) error {
	if err := router.RegisterHandler(protocolsession.MessageListChildren, catalogHandler); err != nil {
		return err
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks,
	} {
		if err := router.RegisterHandler(kind, contentHandler); err != nil {
			return err
		}
	}
	if err := router.RegisterHandler(protocolsession.MessageLaneAttach, laneHandler); err != nil {
		return err
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessagePeerOffer, protocolsession.MessagePeerCandidate,
	} {
		if err := router.RegisterHandler(kind, peerHandler); err != nil {
			return err
		}
	}
	return router.RegisterHandler(protocolsession.MessageCancel, cancelMux{
		catalog: catalogHandler, content: contentHandler, peer: peerHandler,
	})
}

type SenderTerminalSendTransportDisposition string

const (
	SenderTerminalSendTransportAccepted   SenderTerminalSendTransportDisposition = "accepted"
	SenderTerminalSendTransportNotReached SenderTerminalSendTransportDisposition = "not_reached"
	SenderTerminalSendTransportUnsettled  SenderTerminalSendTransportDisposition = "unsettled"
	SenderTerminalSendTransportRejected   SenderTerminalSendTransportDisposition = "rejected_before_acceptance"
	SenderTerminalSendTransportRetired    SenderTerminalSendTransportDisposition = "retired_before_acceptance"
)

type SenderTerminalSendOutcome string

const (
	SenderTerminalSendOutcomeDelivered SenderTerminalSendOutcome = "delivered"
	SenderTerminalSendOutcomeDropped   SenderTerminalSendOutcome = "dropped"
	SenderTerminalSendOutcomeUnknown   SenderTerminalSendOutcome = "unknown"
)

type SenderTerminalSendDecision string

const (
	SenderTerminalSendDecisionDelivered         SenderTerminalSendDecision = "delivered"
	SenderTerminalSendDecisionNaturalRetirement SenderTerminalSendDecision = "natural_retirement"
	SenderTerminalSendDecisionFailed            SenderTerminalSendDecision = "failed"
)

// SenderTerminalSendObserved exposes only stable identities and decisions. The
// terminal body, cryptographic material, and provider-specific error text stay
// below this boundary so production logs cannot leak share content or keys.
type SenderTerminalSendObserved struct {
	ProtocolSessionID    protocolsession.ProtocolSessionID
	Lane                 LaneIdentity
	Settled              bool
	TransportDisposition SenderTerminalSendTransportDisposition
	Outcome              SenderTerminalSendOutcome
	Decision             SenderTerminalSendDecision
}

type SenderTerminalSendObserver interface {
	ObserveSenderTerminalSend(SenderTerminalSendObserved)
}

type SenderTerminalSendObserverFunc func(SenderTerminalSendObserved)

func (function SenderTerminalSendObserverFunc) ObserveSenderTerminalSend(observation SenderTerminalSendObserved) {
	if function != nil {
		function(observation)
	}
}

func observeSenderTerminalSend(
	observer SenderTerminalSendObserver,
	sessionID protocolsession.ProtocolSessionID,
	lane LaneIdentity,
	completion protocolsession.SendCompletion,
	naturallyRetired bool,
) {
	if observer == nil {
		return
	}
	observation := SenderTerminalSendObserved{
		ProtocolSessionID: sessionID,
		Lane:              lane,
		Settled:           completion.Settled,
		Decision:          SenderTerminalSendDecisionFailed,
	}
	switch {
	case !completion.Settled:
		observation.TransportDisposition = SenderTerminalSendTransportUnsettled
	case completion.TransportDisposition == framechannel.SendRetired:
		observation.TransportDisposition = SenderTerminalSendTransportRetired
		observation.Decision = SenderTerminalSendDecisionNaturalRetirement
	case completion.TransportDisposition == framechannel.SendRejected:
		observation.TransportDisposition = SenderTerminalSendTransportRejected
	case completion.TransportDisposition == framechannel.SendAccepted:
		observation.TransportDisposition = SenderTerminalSendTransportAccepted
	default:
		observation.TransportDisposition = SenderTerminalSendTransportNotReached
	}
	switch completion.Outcome {
	case protocolsession.SendOutcomeDelivered:
		observation.Outcome = SenderTerminalSendOutcomeDelivered
		observation.Decision = SenderTerminalSendDecisionDelivered
	case protocolsession.SendOutcomeDropped:
		observation.Outcome = SenderTerminalSendOutcomeDropped
	case protocolsession.SendOutcomeUnknown:
		observation.Outcome = SenderTerminalSendOutcomeUnknown
	}
	if naturallyRetired && observation.Decision != SenderTerminalSendDecisionDelivered {
		observation.Decision = SenderTerminalSendDecisionNaturalRetirement
	}
	// Terminal-send evidence is diagnostic consequence data. Observer failures
	// cannot change the already-owned receipt settlement or stop result.
	func() {
		defer func() { _ = recover() }()
		observer.ObserveSenderTerminalSend(observation)
	}()
}

func (runtime *SenderRuntime) Stop(ctx context.Context, message string) error {
	if runtime == nil {
		return ErrRuntimeClosed
	}
	if ctx == nil {
		return ErrRuntimeConfig
	}
	if err := runtime.BeginStop(ctx, message); err != nil {
		return err
	}
	return runtime.WaitStopped(ctx)
}

func (runtime *SenderRuntime) BeginStop(ctx context.Context, message string) error {
	if runtime == nil {
		return ErrRuntimeClosed
	}
	if ctx == nil {
		return ErrRuntimeConfig
	}
	runtime.stopMu.Lock()
	if runtime.stopStarted {
		runtime.stopMu.Unlock()
		return nil
	}
	if runtime.closeStarted || runtime.ctx.Err() != nil {
		runtime.stopMu.Unlock()
		return ErrRuntimeClosed
	}
	runtime.stopMu.Unlock()
	normalized, err := normalizeTerminalMessage(message)
	if err != nil {
		return err
	}
	runtime.stopMu.Lock()
	if runtime.stopStarted {
		runtime.stopMu.Unlock()
		return nil
	}
	if runtime.closeStarted || runtime.ctx.Err() != nil {
		runtime.stopMu.Unlock()
		return ErrRuntimeClosed
	}
	claim := runtime.claimTermination(runtimeTerminationGracefulStop)
	runtime.stopStarted = true
	runtime.stopDone = make(chan struct{})
	runtime.lanesRegistry.Stop()
	runtime.stopMu.Unlock()
	// Claiming before fanout makes graceful stop the immutable root even when a
	// receipt failure or concurrent lane retirement follows.
	runtime.publishTermination(claim)
	stopContext, cancelStop := context.WithCancel(ctx)
	stopLifecycle := context.AfterFunc(runtime.ctx, cancelStop)
	go runtime.runStop(stopContext, ctx, normalized, func() {
		stopLifecycle()
		cancelStop()
	})
	return nil
}

func (runtime *SenderRuntime) runStop(
	deliveryContext context.Context,
	callerContext context.Context,
	message string,
	releaseContext func(),
) {
	runtime.stopMu.Lock()
	stopDone := runtime.stopDone
	runtime.stopMu.Unlock()
	defer func() {
		releaseContext()
		close(stopDone)
	}()
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: message,
	})
	if err == nil {
		err = runtime.outbound.sendTerminalAll(deliveryContext, callerContext, body)
	}
	runtime.stopMu.Lock()
	runtime.stopErr = err
	runtime.stopMu.Unlock()
	// BeginStop already claimed and published the root. This cancellation ends
	// component ownership without reclassifying terminal-send consequences.
	runtime.cancelContext()
	<-runtime.Done()
}

func (runtime *SenderRuntime) WaitStopped(ctx context.Context) error {
	if runtime == nil {
		return ErrRuntimeClosed
	}
	if ctx == nil {
		return ErrRuntimeConfig
	}
	runtime.stopMu.Lock()
	started, stopDone := runtime.stopStarted, runtime.stopDone
	runtime.stopMu.Unlock()
	if !started {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-runtime.Done():
		}
		if err := runtime.waitComposite(ctx); err != nil {
			return errors.Join(runtime.Err(), err)
		}
		return runtime.Err()
	}
	select {
	case <-ctx.Done():
		return errors.Join(runtime.stopError(), ctx.Err())
	case <-stopDone:
	}
	if err := runtime.waitComposite(ctx); err != nil {
		return errors.Join(runtime.stopError(), runtime.Err(), err)
	}
	return errors.Join(runtime.stopError(), runtime.Err())
}

func (runtime *SenderRuntime) waitComposite(ctx context.Context) error {
	if runtime.compositeDone == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-runtime.compositeDone:
		return nil
	}
}

func (runtime *SenderRuntime) stopError() error {
	runtime.stopMu.Lock()
	defer runtime.stopMu.Unlock()
	return runtime.stopErr
}

func (runtime *SenderRuntime) BeginClose() {
	if runtime == nil {
		return
	}
	runtime.stopMu.Lock()
	claim := runtime.claimTermination(runtimeTerminationForcedClose)
	runtime.closeStarted = true
	runtime.stopMu.Unlock()
	runtime.lanesRegistry.Stop()
	runtime.completeTermination(claim, true)
}

func (runtime *SenderRuntime) WaitClosed() {
	if runtime == nil {
		return
	}
	runtime.waitClosed()
	if runtime.compositeDone != nil {
		<-runtime.compositeDone
	}
}

func (runtime *SenderRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.BeginClose()
	runtime.WaitClosed()
}

func (runtime *SenderRuntime) trackComposite(
	factory *SenderFactory,
	sessionID protocolsession.ProtocolSessionID,
) {
	go func() {
		<-runtime.Done()
		runtime.stopMu.Lock()
		started, stopDone := runtime.stopStarted, runtime.stopDone
		runtime.stopMu.Unlock()
		if started {
			<-stopDone
		}
		// Factory keys remain live until both the core and the optional terminal
		// worker have stopped borrowing senderOutbound's signing-key alias.
		factory.mu.Lock()
		if factory.sessions[sessionID] == runtime {
			delete(factory.sessions, sessionID)
		}
		factory.mu.Unlock()
		close(runtime.compositeDone)
	}()
}
