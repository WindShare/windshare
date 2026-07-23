package sessionruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
)

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
	runtime.stopStarted = true
	runtime.stopDone = make(chan struct{})
	runtime.lanesRegistry.Stop()
	runtime.stopMu.Unlock()
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
	runtime.cancel()
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
	runtime.closeStarted = true
	runtime.stopMu.Unlock()
	runtime.lanesRegistry.Stop()
	runtime.beginClose()
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
