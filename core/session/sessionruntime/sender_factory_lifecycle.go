package sessionruntime

import (
	"context"
	"errors"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func (factory *SenderFactory) Stop(ctx context.Context, message string) error {
	if factory == nil {
		return ErrRuntimeClosed
	}
	if ctx == nil {
		return ErrRuntimeConfig
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
func (factory *SenderFactory) BeginStop(message string) error {
	if factory == nil {
		return ErrRuntimeClosed
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
	factory.cancelAdmissions()
	factory.mu.Unlock()
	go factory.runTerminal(normalized)
	return nil
}

func normalizeTerminalMessage(message string) (string, error) {
	if message == "" {
		message = "Sender stopped"
	}
	if !utf8.ValidString(message) || !norm.NFC.IsNormalString(message) || len(message) > MaximumTerminalMessageBytes {
		return "", ErrRuntimeConfig
	}
	return message, nil
}

func (factory *SenderFactory) runTerminal(message string) {
	factory.terminalConnectivity.StopRecovery()
	factory.admissions.Wait()
	factory.mu.Lock()
	sessions := make([]*SenderRuntime, 0, len(factory.sessions))
	for _, session := range factory.sessions {
		sessions = append(sessions, session)
	}
	factory.mu.Unlock()
	sessionContext, cancelSessions := context.WithTimeout(context.Background(), factory.terminalTimeout)
	results := make(chan error, len(sessions))
	for _, session := range sessions {
		go func() {
			results <- stopFactorySession(sessionContext, session, message)
		}()
	}
	var combined error
	for range sessions {
		combined = errors.Join(combined, <-results)
	}
	cancelSessions()
	// Graceful stop is bounded, but resource ownership is not. Force cancellation
	// after that budget and join every runtime before shared stores or key aliases
	// can be released by the factory owner.
	for _, session := range sessions {
		session.BeginClose()
	}
	for _, session := range sessions {
		session.WaitClosed()
	}
	// Connectivity cleanup runs after the initial session lanes have either
	// delivered terminal or been force-cancelled and fully joined.
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), factory.terminalTimeout)
	combined = errors.Join(combined, factory.terminalConnectivity.Cleanup(cleanupContext))
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
	factory.admissionContext = nil
	factory.cancelAdmissions = nil
	factory.sessions = nil
	factory.mu.Unlock()
	factory.terminalErr = combined
	close(factory.terminalDone)
}

func stopFactorySession(ctx context.Context, session *SenderRuntime, message string) error {
	err := session.BeginStop(ctx, message)
	if errors.Is(err, ErrRuntimeClosed) {
		// The factory map tracks composite resource borrowers, so a naturally
		// ended runtime may remain present after it has lost terminal authority.
		// The force-close join below owns its cleanup; its prior session error is
		// not evidence that this factory stop failed.
		return nil
	}
	if err != nil {
		return err
	}
	return session.WaitStopped(ctx)
}
