package receivercontinuation

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type revisionLease struct {
	mu         sync.Mutex
	runtime    *receiversession.Runtime
	opened     transfer.OpenedRevision
	reopened   *content.RevisionReopenedError
	cleanupErr error
}

func (s *Session) OpenRevision(ctx context.Context, file catalog.FileID) (transfer.OpenedRevision, error) {
	for {
		runtime := s.Runtime()
		dependencies, err := runtime.TransferDependencies()
		if err != nil {
			return transfer.OpenedRevision{}, err
		}
		opened, err := dependencies.OpenRevision(ctx, file)
		if err == nil {
			s.mu.Lock()
			s.leases[opened.Handle] = &revisionLease{runtime: runtime, opened: opened}
			s.mu.Unlock()
			return opened, nil
		}
		if isAuthenticatedSourceOutcome(err) || !runtime.AwaitPathRetirement(ctx) {
			return opened, err
		}
		if err = s.recoverContent(ctx, runtime); err != nil {
			return transfer.OpenedRevision{}, err
		}
	}
}

func (s *Session) ReleaseRevision(ctx context.Context, lease transfer.RevisionHandle) error {
	s.mu.Lock()
	binding := s.leases[lease]
	delete(s.leases, lease)
	s.mu.Unlock()
	if binding == nil {
		return content.ErrInvalidLease
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.runtime.PathsExhausted() {
		return binding.cleanupErr
	}
	dependencies, err := binding.runtime.TransferDependencies()
	if err == nil {
		err = dependencies.ReleaseRevision(ctx, binding.opened.Handle)
	}
	// Path retirement also ends old-generation lease authority when it wins
	// during release. Join that decision before treating cleanup as a failure;
	// protocol rejection and caller shutdown still retain their own errors.
	if err != nil && binding.runtime.AwaitPathRetirement(ctx) {
		return binding.cleanupErr
	}
	return errors.Join(binding.cleanupErr, err)
}

func (s *Session) ReadRange(ctx context.Context, lease transfer.RevisionHandle, descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	s.mu.Lock()
	binding := s.leases[lease]
	s.mu.Unlock()
	if binding == nil {
		return content.ErrInvalidLease
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if err := binding.validateRead(ctx, descriptor); err != nil {
		return err
	}
	remaining := &remainingSink{target: sink, missing: []content.Range{requested}}
	for {
		runtime := s.Runtime()
		if err := binding.refresh(ctx, runtime, descriptor); err != nil {
			retry, retryErr := s.recoverRefreshFailure(ctx, runtime, err)
			if !retry {
				return retryErr
			}
			continue
		}
		dependencies, err := runtime.TransferDependencies()
		if err != nil {
			return err
		}
		if err = readRemainingRanges(ctx, dependencies, binding.opened.Handle, descriptor, remaining); err == nil {
			return nil
		}
		if reopened, ok := errors.AsType[*content.RevisionReopenedError](err); ok {
			binding.reopened = reopened
			return err
		}
		// An authenticated source outcome already proved the source state on this
		// generation; path retirement afterward must not discard that decision or reconnect.
		if isAuthenticatedSourceOutcome(err) || !runtime.AwaitPathRetirement(ctx) {
			return err
		}
		if err = s.recoverContent(ctx, runtime); err != nil {
			return err
		}
	}
}

func (binding *revisionLease) validateRead(ctx context.Context, descriptor content.FileRevisionDescriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if binding.reopened != nil && binding.reopened.Previous() == descriptor {
		return binding.reopened
	}
	if binding.opened.Descriptor != descriptor {
		return transfer.ErrBlockIdentity
	}
	return nil
}

func (s *Session) recoverRefreshFailure(ctx context.Context, runtime *receiversession.Runtime, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// An authenticated source outcome (including RevisionReopenedError) already
	// succeeded on the wire and must never be swallowed by a concurrent path
	// retirement on the refreshed runtime.
	if isAuthenticatedSourceOutcome(err) {
		return false, err
	}
	if !runtime.AwaitPathRetirement(ctx) {
		return false, err
	}
	if err = s.recoverContent(ctx, runtime); err != nil {
		return false, err
	}
	return true, nil
}

func isAuthenticatedSourceOutcome(err error) bool {
	if _, ok := errors.AsType[*content.RevisionReopenedError](err); ok {
		return true
	}
	if boundary, ok := errors.AsType[*fault.BoundaryError](err); ok && boundary != nil {
		if code, isSource := boundary.Fault().SourceCode(); isSource {
			return code == fault.SourceRevisionChanged ||
				code == fault.SourceRevisionInvalidated ||
				code == fault.SourcePermanent
		}
	}
	return false
}

func readRemainingRanges(
	ctx context.Context,
	dependencies receiversession.TransferDependencies,
	handle transfer.RevisionHandle,
	descriptor content.FileRevisionDescriptor,
	remaining *remainingSink,
) error {
	for _, part := range remaining.ranges() {
		if err := dependencies.ReadRange(ctx, handle, descriptor, part, remaining); err != nil {
			return err
		}
	}
	return nil
}

// Only interval metadata survives retry. Verified bytes stay in the existing
// bounded range buffer and output transaction, with no second temporary file.
type remainingSink struct {
	mu      sync.Mutex
	target  transfer.RangeSink
	missing []content.Range
}

func (s *remainingSink) ranges() []content.Range {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]content.Range(nil), s.missing...)
}
func (s *remainingSink) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.target.WriteRange(ctx, offset, data); err != nil {
		return err
	}
	end := offset + uint64(len(data))
	next := make([]content.Range, 0, len(s.missing)+1)
	for _, part := range s.missing {
		if end <= part.Offset || offset >= part.End {
			next = append(next, part)
			continue
		}
		if offset > part.Offset {
			next = append(next, content.Range{Offset: part.Offset, End: offset})
		}
		if end < part.End {
			next = append(next, content.Range{Offset: end, End: part.End})
		}
	}
	s.missing = next
	return nil
}

func (binding *revisionLease) refresh(ctx context.Context, runtime *receiversession.Runtime, descriptor content.FileRevisionDescriptor) error {
	if binding.runtime == runtime {
		return nil
	}
	dependencies, err := runtime.TransferDependencies()
	if err != nil {
		return err
	}
	opened, err := dependencies.OpenRevision(ctx, descriptor.FileID())
	if err != nil {
		return err
	}
	if opened.Descriptor == descriptor {
		binding.runtime, binding.opened = runtime, opened
		binding.reopened = nil
		return nil
	}
	reopened := content.NewRevisionReopenedError(descriptor, opened.Descriptor)
	binding.reopened = reopened
	// One handle owns one immutable descriptor for its lifetime. Release the
	// mismatched new handle immediately; transfer readmission opens a fresh one.
	// Path retirement also ends the discarded lease's authority on the replacement
	// generation; only protocol rejection or caller shutdown remains a cleanup failure.
	if releaseErr := dependencies.ReleaseRevision(ctx, opened.Handle); releaseErr != nil && !runtime.AwaitPathRetirement(ctx) {
		binding.cleanupErr = errors.Join(binding.cleanupErr, releaseErr)
	}
	return reopened
}
