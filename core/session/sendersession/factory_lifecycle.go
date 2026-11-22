package sendersession

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/windshare/windshare/core/session"
	"golang.org/x/text/unicode/norm"
)

// ErrSessionCleanup identifies a local session resource-teardown failure (such
// as failing to release content leases) as distinct from remote relay cleanup.
var ErrSessionCleanup = errors.New("sender session local cleanup failed")

func (factory *Factory) Stop(ctx context.Context, message string) error {
	if factory == nil {
		return session.ErrRuntimeClosed
	}
	if ctx == nil {
		return session.ErrRuntimeConfig
	}
	if err := factory.BeginStop(message); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-factory.terminalDone:
		return factory.terminalErr
	}
}

// BeginStop closes factory admission synchronously, then runs every external
// callback and bounded join in the terminal worker. This makes it safe for
// connectivity cleanup to reenter BeginStop without deadlocking its caller.
func (factory *Factory) BeginStop(message string) error {
	if factory == nil {
		return session.ErrRuntimeClosed
	}
	factory.mu.Lock()
	if factory.stopping {
		factory.mu.Unlock()
		return nil
	}
	factory.mu.Unlock()
	normalized, err := normalizeTerminalMessage(message)
	if err != nil {
		return err
	}
	factory.mu.Lock()
	if factory.stopping {
		factory.mu.Unlock()
		return nil
	}
	factory.stopping = true
	factory.admissions.Seal()
	factory.mu.Unlock()
	go factory.runTerminal(normalized)
	return nil
}

func normalizeTerminalMessage(message string) (string, error) {
	if message == "" {
		message = "Sender stopped"
	}
	if !utf8.ValidString(message) || !norm.NFC.IsNormalString(message) || len(message) > session.MaximumTerminalMessageBytes {
		return "", session.ErrRuntimeConfig
	}
	return message, nil
}

func (factory *Factory) runTerminal(message string) {
	factory.terminalConnectivity.StopRecovery()
	factory.admissions.Close()
	factory.mu.Lock()
	sessions := make([]*Runtime, 0, len(factory.sessions))
	for _, session := range factory.sessions {
		sessions = append(sessions, session)
	}
	factory.mu.Unlock()
	peerTimeout := min(factory.terminalTimeout, DefaultPeerTerminalTimeout)
	sessionContext, cancelSessions := context.WithTimeout(context.Background(), peerTimeout)
	var peerNotice sync.WaitGroup
	for _, current := range sessions {
		peerNotice.Go(func() {
			// Delivering the goodbye frame to arbitrary remote peers is best-effort:
			// an offline peer cannot acknowledge it, and per-lane delivery outcomes
			// are recorded via TerminalSendObserver without failing share shutdown.
			_ = stopFactorySession(sessionContext, current, message)
		})
	}
	peerNotice.Wait()
	cancelSessions()
	// Graceful stop is bounded, but resource ownership is not. Force cancellation
	// after that budget and join every runtime before shared stores or key aliases
	// can be released by the factory owner.
	for _, current := range sessions {
		current.BeginClose()
	}
	var localCleanupErr error
	for _, current := range sessions {
		current.WaitClosed()
		localCleanupErr = errors.Join(localCleanupErr, current.cleanupError())
	}
	if localCleanupErr != nil {
		localCleanupErr = fmt.Errorf("%w: %w", ErrSessionCleanup, localCleanupErr)
	}
	// Connectivity cleanup runs after the initial session lanes have either
	// delivered terminal or been force-cancelled and fully joined.
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), factory.terminalTimeout)
	combined := errors.Join(localCleanupErr, factory.terminalConnectivity.Cleanup(cleanupContext))
	cancelCleanup()
	factory.mu.Lock()
	clear(factory.authKey)
	clear(factory.privateKey)
	factory.authKey = nil
	factory.privateKey = nil
	factory.catalog = nil
	factory.content = nil
	factory.peers = nil
	factory.replay = nil
	factory.random = nil
	factory.laneIDs = nil
	factory.now = nil
	factory.terminalConnectivity = nil
	factory.terminalObserver = nil
	factory.sessions = nil
	factory.mu.Unlock()
	factory.terminalErr = combined
	close(factory.terminalDone)
}

func stopFactorySession(ctx context.Context, runtime *Runtime, message string) error {
	err := runtime.BeginStop(ctx, message)
	if errors.Is(err, session.ErrRuntimeClosed) {
		// The factory map tracks composite resource borrowers, so a naturally
		// ended runtime may remain present after it has lost terminal authority.
		// The force-close join below owns its cleanup; its prior session error is
		// not evidence that this factory stop failed.
		return nil
	}
	if err != nil {
		return err
	}
	return runtime.WaitStopped(ctx)
}
