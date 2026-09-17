package contentflow

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

var ErrBlockResponseInactivity = errors.New("block request made no progress while awaiting its first fragment")

// BlockReceivePhase distinguishes wire queue delay from a stalled assembly.
type BlockReceivePhase string

const (
	BlockAwaitingFirstFragment BlockReceivePhase = "awaiting_first_fragment"
	BlockReceivingFragments    BlockReceivePhase = "receiving_fragments"
)

// BlockWaitTimeout preserves the decision behind an operation's retirement.
type BlockWaitTimeout struct {
	Phase         BlockReceivePhase
	Waited        time.Duration
	QueueProgress uint64
}

func (failure *BlockWaitTimeout) Error() string { return failure.Unwrap().Error() }
func (failure *BlockWaitTimeout) Unwrap() error {
	if failure.Phase == BlockAwaitingFirstFragment {
		return ErrBlockResponseInactivity
	}
	return ErrFragmentInactivity
}

// BlockResponseQueue belongs to one content lane. Its zero value is ready to use.
// Only work already ahead of a request can justify its first-response wait:
// later requests, heartbeats and duplicate fragments cannot keep it alive.
type BlockResponseQueue struct {
	mu      sync.Mutex
	pending list.List
}

type BlockResponseWait struct {
	queue         *BlockResponseQueue
	element       *list.Element
	ctx           context.Context
	cancel        context.CancelCauseFunc
	timer         *time.Timer
	started       time.Time
	deadline      time.Time
	inactivity    time.Duration
	phase         BlockReceivePhase
	reading       bool
	queueProgress uint64
}

func (queue *BlockResponseQueue) Begin(parent context.Context, inactivity time.Duration) *BlockResponseWait {
	ctx, cancel := context.WithCancelCause(parent)
	now := time.Now()
	wait := &BlockResponseWait{
		queue: queue, ctx: ctx, cancel: cancel, started: now,
		deadline: now.Add(inactivity), inactivity: inactivity, phase: BlockAwaitingFirstFragment, reading: true,
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	wait.element = queue.pending.PushBack(wait)
	wait.timer = time.AfterFunc(inactivity, wait.checkDeadline)
	return wait
}

func (wait *BlockResponseWait) Context() context.Context { return wait.ctx }

// Progress is called only after accepting a new authenticated fragment. The
// finite predecessor set makes queue allowances bounded by useful existing work,
// without deriving a correctness deadline from a noisy throughput estimate.
func (wait *BlockResponseWait) Progress() {
	wait.queue.mu.Lock()
	defer wait.queue.mu.Unlock()
	if wait.element == nil || wait.ctx.Err() != nil {
		return
	}
	now := time.Now()
	wait.phase = BlockReceivingFragments
	wait.deadline = now.Add(wait.inactivity)
	for element := wait.element.Next(); element != nil; element = element.Next() {
		later := element.Value.(*BlockResponseWait)
		if later.phase == BlockAwaitingFirstFragment && later.ctx.Err() == nil && now.Before(later.deadline) {
			later.deadline = now.Add(later.inactivity)
			later.queueProgress++
		}
	}
}

// Suspend excludes local validation from the next-message inactivity clock.
func (wait *BlockResponseWait) Suspend() {
	wait.queue.mu.Lock()
	defer wait.queue.mu.Unlock()
	wait.reading = false
	wait.timer.Stop()
}

func (wait *BlockResponseWait) Resume() {
	wait.queue.mu.Lock()
	defer wait.queue.mu.Unlock()
	if wait.element != nil && !wait.reading && wait.ctx.Err() == nil {
		wait.reading = true
		wait.timer.Reset(time.Until(wait.deadline))
	}
}

func (wait *BlockResponseWait) checkDeadline() {
	wait.queue.mu.Lock()
	defer wait.queue.mu.Unlock()
	if wait.element == nil || !wait.reading || wait.ctx.Err() != nil {
		return
	}
	now := time.Now()
	if remaining := wait.deadline.Sub(now); remaining > 0 {
		// Queue progress can extend the deadline without waking the reader.
		wait.timer.Reset(remaining)
		return
	}
	wait.cancel(&BlockWaitTimeout{
		Phase: wait.phase, Waited: now.Sub(wait.started), QueueProgress: wait.queueProgress,
	})
}

func (wait *BlockResponseWait) Close() {
	wait.queue.mu.Lock()
	defer wait.queue.mu.Unlock()
	if wait.element == nil {
		return
	}
	wait.timer.Stop()
	wait.queue.pending.Remove(wait.element)
	wait.element = nil
	wait.cancel(context.Canceled)
}
