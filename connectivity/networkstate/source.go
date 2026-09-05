package networkstate

import (
	"context"
	"sync"
	"time"
)

const DefaultPollInterval = time.Second
const DefaultDebounce = 500 * time.Millisecond
const ResumeGap = 10 * time.Second

type Source interface {
	Snapshot(context.Context) (State, error)
}
type SystemSource struct{}
type Monitor struct {
	mu       sync.Mutex
	source   Source
	observer Observer
	lastPoll time.Time
	resume   uint64
	current  Snapshot
}

func NewMonitor(source Source, debounce time.Duration) *Monitor {
	if source == nil {
		source = SystemSource{}
	}
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	return &Monitor{source: source, observer: Observer{Debounce: debounce}}
}
func (m *Monitor) Current() Snapshot { m.mu.Lock(); defer m.mu.Unlock(); return m.current }

// Poll detects resume by an unexpectedly long observation gap. Platform route
// and address facts are read together and never applied to an existing snapshot.
func (m *Monitor) Poll(ctx context.Context, now time.Time) (Snapshot, bool, error) {
	state, err := m.source.Snapshot(ctx)
	if err != nil {
		return m.Current(), false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.lastPoll.IsZero() && now.Sub(m.lastPoll) > ResumeGap {
		m.resume++
	}
	m.lastPoll = now
	state.ResumeSequence += m.resume
	snapshot, changed := m.observer.Observe(state, now)
	m.current = snapshot
	return snapshot, changed, nil
}
