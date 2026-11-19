package receivercontinuation

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
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
		if !runtime.AwaitPathRetirement(ctx) {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if binding.reopened != nil && binding.reopened.Previous() == descriptor {
		return binding.reopened
	}
	if binding.opened.Descriptor != descriptor {
		return transfer.ErrBlockIdentity
	}
	remaining := &remainingSink{target: sink, missing: []content.Range{requested}}
	for {
		runtime := s.Runtime()
		if err := binding.refresh(ctx, runtime, descriptor); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if runtime.AwaitPathRetirement(ctx) {
				if err = s.recoverContent(ctx, runtime); err == nil {
					continue
				}
			}
			return err
		}
		dependencies, err := runtime.TransferDependencies()
		if err != nil {
			return err
		}
		for _, part := range remaining.ranges() {
			if err = dependencies.ReadRange(ctx, binding.opened.Handle, descriptor, part, remaining); err != nil {
				break
			}
		}
		if err == nil {
			return nil
		}
		if !runtime.AwaitPathRetirement(ctx) {
			if reopened, ok := err.(*content.RevisionReopenedError); ok {
				binding.reopened = reopened
				if reopened.Comparison().AllowsReuse() {
					binding.opened.Descriptor = reopened.Current()
				}
			}
			return err
		}
		if err = s.recoverContent(ctx, runtime); err != nil {
			return err
		}
	}
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
	if reopened.Comparison().AllowsReuse() {
		// The retired generation no longer owns a live lease. Keep the new
		// generation's handle across the output's durable pause and readmission.
		binding.runtime, binding.opened = runtime, opened
	} else {
		binding.cleanupErr = dependencies.ReleaseRevision(ctx, opened.Handle)
	}
	return reopened
}
