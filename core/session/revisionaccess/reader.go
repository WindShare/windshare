// Package revisionaccess keeps a receiver's immutable file identity independent
// of the expiring wire capabilities used to read it.
package revisionaccess

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const cleanupTimeout = 5 * time.Second

var ErrClosed = errors.New("revision reader closed")

type Lease struct {
	LeaseID    content.LeaseID
	Descriptor content.FileRevisionDescriptor
}

func NewLease(id content.LeaseID, descriptor content.FileRevisionDescriptor) (Lease, error) {
	if id.IsZero() || descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return Lease{}, transfer.ErrRevisionIdentity
	}
	return Lease{LeaseID: id, Descriptor: descriptor}, nil
}

// Source owns wire operations and their authenticated error classification.
type Source interface {
	OpenLease(context.Context, catalog.FileID) (Lease, error)
	ReleaseLease(context.Context, content.LeaseID) error
	ReadLeaseRange(context.Context, content.LeaseID, content.FileRevisionDescriptor, content.Range, transfer.RangeSink) error
}

type Reader struct {
	source   Source
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	bindings map[transfer.RevisionHandle]*binding
}

type binding struct {
	gate       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	lease      Lease
	failure    error
	cleanupErr error
}

func New(ctx context.Context, source Source) *Reader {
	lifetime, cancel := context.WithCancel(ctx)
	return &Reader{source: source, ctx: lifetime, cancel: cancel, bindings: make(map[transfer.RevisionHandle]*binding)}
}

func (r *Reader) OpenRevision(ctx context.Context, file catalog.FileID) (transfer.OpenedRevision, error) {
	ctx, cancel := linkedContext(ctx, r.ctx)
	defer cancel()
	if r.ctx.Err() != nil {
		return transfer.OpenedRevision{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return transfer.OpenedRevision{}, err
	}
	lease, err := r.source.OpenLease(ctx, file)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	if _, err = NewLease(lease.LeaseID, lease.Descriptor); err == nil && lease.Descriptor.FileID() != file {
		err = transfer.ErrRevisionIdentity
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return transfer.OpenedRevision{}, errors.Join(err, r.release(ctx, lease.LeaseID))
	}
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return transfer.OpenedRevision{}, errors.Join(ErrClosed, r.release(ctx, lease.LeaseID))
	}
	defer r.mu.Unlock()
	var handle transfer.RevisionHandle
	for handle.IsZero() || r.bindings[handle] != nil {
		_, _ = rand.Read(handle[:])
	}
	lifetime, stop := context.WithCancel(r.ctx)
	r.bindings[handle] = &binding{gate: make(chan struct{}, 1), ctx: lifetime, cancel: stop, lease: lease}
	return transfer.NewOpenedRevision(handle, lease.Descriptor)
}

func (r *Reader) ReadRange(ctx context.Context, handle transfer.RevisionHandle, descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	r.mu.Lock()
	b := r.bindings[handle]
	r.mu.Unlock()
	if r.ctx.Err() != nil {
		return ErrClosed
	}
	if b == nil {
		return content.ErrInvalidLease
	}
	ctx, cancel := linkedContext(ctx, b.ctx)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.unlock()
	if err := b.validateRead(descriptor, requested, sink); err != nil {
		return err
	}
	return r.readBindingRange(ctx, b, descriptor, requested, sink)
}

func (b *binding) validateRead(descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	// Linked-context cancellation is delivered asynchronously; the binding's
	// owner remains the synchronous admission fence for queued reads.
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.lease.Descriptor != descriptor {
		return transfer.ErrBlockIdentity
	}
	if b.failure != nil {
		return b.failure
	}
	if sink == nil || requested.Offset >= requested.End || requested.End > descriptor.ExactSize() {
		return transfer.ErrInvalidDemand
	}
	return nil
}

func (r *Reader) readBindingRange(ctx context.Context, b *binding, descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	remaining := &progressSink{target: sink, next: requested.Offset, end: requested.End}
	replacedAt, replaced := uint64(0), false
	for remaining.next < requested.End {
		start := remaining.next
		err := r.source.ReadLeaseRange(ctx, b.lease.LeaseID, descriptor, content.Range{Offset: start, End: requested.End}, remaining)
		if err == nil {
			if remaining.next != requested.End {
				return transfer.ErrBlockIdentity
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// An in-flight block can see InvalidLease after the sender detaches an
		// expired lease but before its renewal rejection reaches us. Both lease
		// failures permit reauthorization, never a change of file identity.
		if (!errors.Is(err, content.ErrLeaseExpired) && !errors.Is(err, content.ErrInvalidLease)) || remaining.failure != nil {
			return err
		}
		if remaining.next == requested.End {
			return nil
		}
		// A peer repeatedly expiring fresh leases without useful bytes must not spin.
		if replaced && replacedAt == remaining.next {
			return err
		}
		replacementErr := r.replace(ctx, b)
		if err := ctx.Err(); err != nil {
			return err
		}
		if replacementErr != nil {
			return replacementErr
		}
		replacedAt, replaced = remaining.next, true
	}
	return nil
}

func (r *Reader) replace(ctx context.Context, b *binding) error {
	next, err := r.source.OpenLease(ctx, b.lease.Descriptor.FileID())
	if err != nil {
		return err
	}
	if next.LeaseID.IsZero() || next.LeaseID == b.lease.LeaseID {
		b.failure = transfer.ErrRevisionIdentity
		return b.failure
	}
	if err := ctx.Err(); err != nil {
		b.recordCleanup(r.release(ctx, next.LeaseID))
		return err
	}
	previous := b.lease
	if next.Descriptor != previous.Descriptor {
		// One RevisionHandle owns one immutable descriptor for its lifetime.
		// Release the mismatched lease immediately and fence stale reads on this
		// handle so transfer readmission opens a fresh handle cleanly.
		reopened := content.NewRevisionReopenedError(previous.Descriptor, next.Descriptor)
		b.failure = reopened
		b.recordCleanup(r.release(ctx, next.LeaseID))
		return reopened
	}
	b.lease = next
	b.recordCleanup(r.release(ctx, previous.LeaseID))
	return nil
}

func (r *Reader) ReleaseRevision(ctx context.Context, handle transfer.RevisionHandle) error {
	r.mu.Lock()
	b := r.bindings[handle]
	delete(r.bindings, handle)
	r.mu.Unlock()
	if r.ctx.Err() != nil {
		return ErrClosed
	}
	if b == nil {
		return content.ErrInvalidLease
	}
	b.cancel()
	acquireCtx, cancelAcquire := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancelAcquire()
	if err := b.acquire(acquireCtx); err != nil {
		return err
	}
	defer b.unlock()
	// Explicit handle release honors caller cancellation and shorter caller
	// deadlines so tearing down a canceled job relinquishes local state without
	// blocking on a remote RPC.
	return errors.Join(b.cleanupErr, r.releaseCallerScoped(ctx, b.lease.LeaseID))
}

// A download can replace many leases. Keep the first unconfirmed release for
// the owner's cleanup report without growing state or overriding a read result.
func (b *binding) recordCleanup(err error) {
	if b.cleanupErr == nil {
		b.cleanupErr = err
	}
}

func (r *Reader) Stop() {
	r.cancel()
	r.mu.Lock()
	clear(r.bindings)
	r.mu.Unlock()
}

func (r *Reader) release(ctx context.Context, id content.LeaseID) error {
	if id.IsZero() {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return r.releaseWithContext(cleanup, id)
}

func (r *Reader) releaseCallerScoped(ctx context.Context, id content.LeaseID) error {
	if id.IsZero() {
		return nil
	}
	cleanup, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	return r.releaseWithContext(cleanup, id)
}

func (r *Reader) releaseWithContext(ctx context.Context, id content.LeaseID) error {
	err := r.source.ReleaseLease(ctx, id)
	if errors.Is(err, content.ErrLeaseExpired) || errors.Is(err, content.ErrInvalidLease) {
		return nil
	}
	return err
}

func (b *binding) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			b.unlock()
			return err
		}
		return nil
	}
}
func (b *binding) unlock() { <-b.gate }

func linkedContext(parent, lifetime context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

type progressSink struct {
	target  transfer.RangeSink
	next    uint64
	end     uint64
	failure error
}

func (s *progressSink) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	if offset != s.next || len(data) == 0 || uint64(len(data)) > s.end-s.next {
		s.failure = transfer.ErrBlockIdentity
		return s.failure
	}
	if err := s.target.WriteRange(ctx, offset, data); err != nil {
		s.failure = err
		return err
	}
	s.next += uint64(len(data))
	return nil
}
