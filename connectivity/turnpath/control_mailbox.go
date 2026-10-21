package turnpath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/windshare/windshare/core/session/turncontrol"
)

const controlCommandCapacity = 32

var errControlStopped = errors.New("TURN control owner stopped")

// Only commands enter this mailbox. Cumulative reports belong to the bounded
// path state, so telemetry cannot delay or displace a lifecycle transition.
type controlMailbox struct {
	mu      sync.Mutex
	queue   []turncontrol.Message
	wake    chan struct{}
	changed chan struct{}
	closed  bool
}

func newControlMailbox() *controlMailbox {
	return &controlMailbox{wake: make(chan struct{}, 1), changed: make(chan struct{})}
}

// Acceptance confirms local ownership, not remote execution. Saturation applies
// cancellable backpressure instead of turning overload into silent success or a
// session-fatal error. The protocol dispatcher supplies its session lifetime.
func (m *controlMailbox) put(ctx context.Context, message turncontrol.Message) error {
	waited := false
	for {
		m.mu.Lock()
		if err := m.checkLocked(ctx); err != nil {
			m.mu.Unlock()
			return err
		}
		if m.replaceInventory(message) {
			m.mu.Unlock()
			return nil
		}
		if len(m.queue) < controlCommandCapacity {
			m.queue = append(m.queue, message)
			m.signalLocked()
			m.mu.Unlock()
			return nil
		}
		changed := m.changed
		m.mu.Unlock()
		if !waited {
			slog.Debug("TURN control backpressure", "session_id", fmt.Sprintf("%x", message.Header().SessionID),
				"message_type", fmt.Sprintf("%T", message), "capacity", controlCommandCapacity)
			waited = true
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-changed:
		}
	}
}

func (m *controlMailbox) check(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkLocked(ctx)
}

func (m *controlMailbox) checkLocked(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if m.closed {
		return errControlStopped
	}
	return nil
}

// Adjacent inventory snapshots supersede one another. Never coalesce across a
// command: demand cancellation and grant/retire ordering are lifecycle barriers.
func (m *controlMailbox) replaceInventory(message turncontrol.Message) bool {
	next, ok := message.(turncontrol.Available)
	if !ok || len(m.queue) == 0 {
		return false
	}
	index := len(m.queue) - 1
	previous, ok := m.queue[index].(turncontrol.Available)
	if !ok || previous.Authority.SessionID != next.Authority.SessionID ||
		previous.Authority.OwnerPathID != next.Authority.OwnerPathID {
		return false
	}
	if next.Authority.Sequence > previous.Authority.Sequence {
		m.queue[index] = next
	}
	return true
}

func (m *controlMailbox) take() (turncontrol.Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) == 0 {
		return nil, false
	}
	message := m.queue[0]
	m.queue[0] = nil
	m.queue = m.queue[1:]
	close(m.changed)
	m.changed = make(chan struct{})
	if len(m.queue) != 0 {
		m.signalLocked()
	}
	return message, true
}

func (m *controlMailbox) signalLocked() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *controlMailbox) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		clear(m.queue)
		m.queue = nil
		close(m.changed)
	}
}
