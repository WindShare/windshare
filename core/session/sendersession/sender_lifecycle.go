package sendersession

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationCancelHandler interface {
	HandleMessage(context.Context, protocolsession.Operation, protocolsession.Message) error
}

type cancelMux struct {
	catalog operationCancelHandler
	content operationCancelHandler
	peer    PeerHandler
}

func (mux cancelMux) HandleMessage(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
) error {
	operationID, ok := message.OperationID()
	if !ok {
		return session.ErrRuntimeConfig
	}
	if _, err := contentflow.DecodeCancelReason(message.Body()); err != nil {
		return err
	}
	if op.IsZero() || op.ID() != operationID {
		return session.ErrOperationMissing
	}
	requestKind, ok := op.RequestKind()
	if !ok {
		// A preemptive CANCEL has no work owner. Delayed handlers independently
		// suppress its now-tombstoned operation before starting service work.
		return nil
	}
	switch requestKind {
	case protocolsession.MessageListChildren:
		return mux.catalog.HandleMessage(ctx, op, message)
	case protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks:
		return mux.content.HandleMessage(ctx, op, message)
	case protocolsession.MessagePeerOffer:
		return mux.peer.Cancel(ctx, op)
	case protocolsession.MessageLaneAttach:
		return nil
	default:
		return session.ErrRuntimeConfig
	}
}

func registerSenderHandlers(
	router interface {
		RegisterHandler(protocolsession.MessageKind, protocolsession.MessageHandler) error
	},
	catalogHandler *catalogHandler,
	contentHandler *contentflow.SenderHandler,
	laneHandler *laneGrantHandler,
	peerHandler PeerHandler,
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

func (runtime *Runtime) Stop(ctx context.Context, message string) error {
	if runtime == nil {
		return session.ErrRuntimeClosed
	}
	if ctx == nil {
		return session.ErrRuntimeConfig
	}
	if err := runtime.BeginStop(ctx, message); err != nil {
		return err
	}
	return runtime.WaitStopped(ctx)
}

func (runtime *Runtime) BeginStop(ctx context.Context, message string) error {
	if runtime == nil {
		return session.ErrRuntimeClosed
	}
	if ctx == nil {
		return session.ErrRuntimeConfig
	}
	runtime.stopMu.Lock()
	if runtime.stopStarted {
		runtime.stopMu.Unlock()
		return nil
	}
	if runtime.closeStarted || runtime.Lifetime().Err() != nil {
		runtime.stopMu.Unlock()
		return session.ErrRuntimeClosed
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
	if runtime.closeStarted || runtime.Lifetime().Err() != nil {
		runtime.stopMu.Unlock()
		return session.ErrRuntimeClosed
	}
	claim := runtime.core.ClaimTermination(sessioncore.TerminationGracefulStop)
	runtime.stopStarted = true
	runtime.stopDone = make(chan struct{})
	runtime.lanesRegistry.Stop()
	runtime.stopMu.Unlock()
	// Claiming before fanout makes graceful stop the immutable root even when a
	// receipt failure or concurrent lane retirement follows.
	claim.Publish()
	go runtime.runStop(ctx, normalized)
	return nil
}

func (runtime *Runtime) runStop(
	callerContext context.Context,
	message string,
) {
	runtime.stopMu.Lock()
	stopDone := runtime.stopDone
	runtime.stopMu.Unlock()
	defer close(stopDone)
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: session.SessionStoppedCode, Message: message,
	})
	if err == nil {
		err = runtime.outbound.SendTerminalAll(callerContext, body)
	}
	runtime.stopMu.Lock()
	runtime.stopErr = err
	runtime.stopMu.Unlock()
	// BeginStop already claimed and published the root. This cancellation ends
	// component ownership without reclassifying terminal-send consequences.
	runtime.core.CancelLifetime()
	<-runtime.Done()
}

func (runtime *Runtime) WaitStopped(ctx context.Context) error {
	if runtime == nil {
		return session.ErrRuntimeClosed
	}
	if ctx == nil {
		return session.ErrRuntimeConfig
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

func (runtime *Runtime) waitComposite(ctx context.Context) error {
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

func (runtime *Runtime) stopError() error {
	runtime.stopMu.Lock()
	defer runtime.stopMu.Unlock()
	return runtime.stopErr
}

func (runtime *Runtime) recordCleanupError(err error) {
	if runtime == nil || err == nil {
		return
	}
	runtime.stopMu.Lock()
	runtime.cleanupErr = errors.Join(runtime.cleanupErr, err)
	runtime.stopMu.Unlock()
}

func (runtime *Runtime) cleanupError() error {
	if runtime == nil {
		return nil
	}
	runtime.stopMu.Lock()
	defer runtime.stopMu.Unlock()
	return runtime.cleanupErr
}

func (runtime *Runtime) BeginClose() {
	if runtime == nil {
		return
	}
	runtime.stopMu.Lock()
	claim := runtime.core.ClaimTermination(sessioncore.TerminationForcedClose)
	runtime.closeStarted = true
	runtime.stopMu.Unlock()
	runtime.lanesRegistry.Stop()
	runtime.core.CancelLifetime()
	claim.Publish()
}

func (runtime *Runtime) WaitClosed() {
	if runtime == nil {
		return
	}
	<-runtime.Done()
	if runtime.compositeDone != nil {
		<-runtime.compositeDone
	}
}

func (runtime *Runtime) Close() {
	if runtime == nil {
		return
	}
	runtime.BeginClose()
	runtime.WaitClosed()
}

func (runtime *Runtime) trackComposite(
	factory *Factory,
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
