package receivercontinuation

import (
	"context"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

type revisionLease struct {
	mu      sync.Mutex
	runtime *sessionruntime.ReceiverRuntime
	opened  transfer.OpenedRevision
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
			s.leases[opened.LeaseID] = &revisionLease{runtime: runtime, opened: opened}
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

func (s *Session) ReleaseRevision(ctx context.Context, lease content.LeaseID) error {
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
		return nil
	}
	dependencies, err := binding.runtime.TransferDependencies()
	if err != nil {
		return err
	}
	return dependencies.ReleaseRevision(ctx, binding.opened.LeaseID)
}

func (s *Session) ReadRange(ctx context.Context, lease content.LeaseID, descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	s.mu.Lock()
	binding := s.leases[lease]
	s.mu.Unlock()
	if binding == nil {
		return content.ErrInvalidLease
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.opened.Descriptor != descriptor {
		return transfer.ErrBlockIdentity
	}
	remaining := &remainingSink{target: sink, missing: []content.Range{requested}}
	for {
		runtime := s.Runtime()
		if err := binding.refresh(ctx, runtime, descriptor); err != nil {
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
			if err = dependencies.ReadRange(ctx, binding.opened.LeaseID, descriptor, part, remaining); err != nil {
				break
			}
		}
		if err == nil {
			return nil
		}
		if !runtime.AwaitPathRetirement(ctx) {
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

func (binding *revisionLease) refresh(ctx context.Context, runtime *sessionruntime.ReceiverRuntime, descriptor content.FileRevisionDescriptor) error {
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
	// A fresh lease does not certify old bytes unless the authenticated immutable
	// revision and geometry are identical.
	if opened.Descriptor != descriptor {
		_ = dependencies.ReleaseRevision(ctx, opened.LeaseID)
		return content.ErrRevisionDrift
	}
	binding.runtime = runtime
	binding.opened = opened
	return nil
}
