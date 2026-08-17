// Package observationbridge owns CLI-side execution of bounded producer
// observation streams. Producers retain only queue and completion authority;
// projection, cancellation, and join accounting stay at the command boundary.
package observationbridge

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

type LossSink interface {
	ReportObserverLoss(clievent.ObserverLossCategory, clievent.ObserverLossReason, uint64) bool
}

type cumulativeLossKey[Source comparable] struct {
	source   Source
	category clievent.ObserverLossCategory
	reason   clievent.ObserverLossReason
}

// CumulativeLosses keeps producer identities distinct until the narrower CLI
// loss schema. Turning snapshots into deltas here makes repeated completion
// reports idempotent without coupling producer packages to command events.
type CumulativeLosses[Source comparable] struct {
	mu       sync.Mutex
	sink     LossSink
	reported map[cumulativeLossKey[Source]]uint64
}

func NewCumulativeLosses[Source comparable](sink LossSink) *CumulativeLosses[Source] {
	if sink == nil {
		return nil
	}
	return &CumulativeLosses[Source]{sink: sink}
}

func (losses *CumulativeLosses[Source]) Report(
	source Source,
	category clievent.ObserverLossCategory,
	reason clievent.ObserverLossReason,
	cumulative uint64,
) {
	if losses == nil || cumulative == 0 {
		return
	}
	key := cumulativeLossKey[Source]{source: source, category: category, reason: reason}
	losses.mu.Lock()
	if losses.reported == nil {
		losses.reported = make(map[cumulativeLossKey[Source]]uint64)
	}
	previous := losses.reported[key]
	if cumulative <= previous {
		losses.mu.Unlock()
		return
	}
	losses.reported[key] = cumulative
	losses.mu.Unlock()
	losses.sink.ReportObserverLoss(category, reason, cumulative-previous)
}

// Status describes only facts knowable at the reader join cut. Joined=false
// deliberately does not imply that cancellation terminated an active projector.
type Status struct {
	Forwarded uint64
	Buffered  uint64
	Active    bool
	Joined    bool
}

// PublicationGate linearizes reader cancellation with command publication.
// Projection may already be active when Revoke returns, but it cannot acquire
// publication authority afterward.
type PublicationGate struct {
	mu      sync.Mutex
	revoked bool
}

func (gate *PublicationGate) Commit(ctx context.Context, publish func() bool) bool {
	if publish == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if gate == nil {
		return ctx.Err() == nil && publish()
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.revoked || ctx.Err() != nil {
		return false
	}
	return publish()
}

func (gate *PublicationGate) Revoke() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	gate.revoked = true
	gate.mu.Unlock()
}

// Reader owns exactly one goroutine for one enabled stream. The producer can
// fill its bounded early prefix before Start because receiving begins only here.
type Reader[T any] struct {
	stream  <-chan T
	project func(context.Context, T)
	gate    *PublicationGate
	cancel  context.CancelFunc
	done    chan struct{}

	forwarded atomic.Uint64
	active    atomic.Bool
	joinOnce  sync.Once
	status    Status
}

func Start[T any](stream <-chan T, gate *PublicationGate, project func(context.Context, T)) *Reader[T] {
	if stream == nil || project == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := &Reader[T]{
		stream: stream, project: project, gate: gate, cancel: cancel, done: make(chan struct{}),
	}
	go reader.run(ctx)
	return reader
}

func (reader *Reader[T]) run(ctx context.Context) {
	defer close(reader.done)
	for {
		// Check cancellation before selecting the next retained value. Without this
		// priority, a canceled reader and a buffered stream are both ready and the
		// scheduler may begin another projection after the join cut.
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case observation, open := <-reader.stream:
			if !open {
				return
			}
			reader.active.Store(true)
			reader.project(ctx, observation)
			reader.active.Store(false)
			saturatingIncrement(&reader.forwarded)
		}
	}
}

// Join waits only within ctx, then revokes future publication and cancels the
// reader. It never performs an unconditional second wait for a blocked projector.
func (reader *Reader[T]) Join(ctx context.Context) Status {
	if reader == nil {
		return Status{Joined: true}
	}
	reader.joinOnce.Do(func() {
		joined := reader.wait(ctx)
		reader.cancel()
		reader.gate.Revoke()
		reader.status = Status{
			Forwarded: reader.forwarded.Load(),
			Buffered:  uint64(len(reader.stream)),
			Active:    reader.active.Load(),
			Joined:    joined,
		}
	})
	return reader.status
}

func (reader *Reader[T]) wait(ctx context.Context) bool {
	select {
	case <-reader.done:
		return true
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-reader.done:
		return true
	case <-ctx.Done():
		// Prefer a completed reader when deadline delivery raced its final return.
		select {
		case <-reader.done:
			return true
		default:
			return false
		}
	}
}

func saturatingIncrement(counter *atomic.Uint64) {
	for {
		current := counter.Load()
		if current == ^uint64(0) || counter.CompareAndSwap(current, current+1) {
			return
		}
	}
}
