// Package receivercontinuation retains transfer dependencies across authenticated
// protocol generations without extending the lifetime of any session authority.
package receivercontinuation

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

var ErrReplacement = errors.New("receiver replacement violates authenticated session continuity")

type Replace func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error)

// Session owns the one replacement flight shared by discovery, revision, range,
// and quota-wait consumers. Output transactions belong to the unchanged job.
type Session struct {
	downloadMetrics *downloadmetrics.Metrics
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	current         *sessionruntime.ReceiverRuntime
	replace         Replace
	flight          chan struct{}
	failure         error
	leases          map[content.LeaseID]*revisionLease
}

func New(ctx context.Context, current *sessionruntime.ReceiverRuntime, replace Replace) (*Session, error) {
	if ctx == nil || current == nil || replace == nil {
		return nil, ErrReplacement
	}
	lifetime, cancel := context.WithCancel(ctx)
	return &Session{ctx: lifetime, cancel: cancel, current: current, replace: replace, leases: make(map[content.LeaseID]*revisionLease)}, nil
}

func (s *Session) BindDownloadMetrics(metrics *downloadmetrics.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadMetrics = metrics
}
func (s *Session) recoverContent(ctx context.Context, previous *sessionruntime.ReceiverRuntime) error {
	s.mu.Lock()
	metrics := s.downloadMetrics
	s.mu.Unlock()
	if metrics != nil {
		defer metrics.Pending()()
	}
	return s.recover(ctx, previous)
}

func (s *Session) Runtime() *sessionruntime.ReceiverRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}
func (s *Session) Close() {
	s.cancel()
	s.mu.Lock()
	flight := s.flight
	s.mu.Unlock()
	if flight != nil {
		<-flight
	}
}

func (s *Session) recover(ctx context.Context, previous *sessionruntime.ReceiverRuntime) error {
	s.mu.Lock()
	if s.current != previous {
		s.mu.Unlock()
		return nil
	}
	if s.failure != nil {
		err := s.failure
		s.mu.Unlock()
		return previous.PathRecoveryFailure(err)
	}
	if !previous.PathsExhausted() {
		s.mu.Unlock()
		return sessionruntime.ErrRuntimeClosed
	}
	if s.flight == nil {
		s.flight = make(chan struct{})
		go s.replaceGeneration(previous, s.flight)
	}
	flight := s.flight
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-flight:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return previous.PathRecoveryFailure(s.failure)
	}
	return nil
}

func (s *Session) replaceGeneration(previous *sessionruntime.ReceiverRuntime, flight chan struct{}) {
	next, err := s.replace(s.ctx, previous)
	if err == nil && (next == nil || next.ProtocolSessionID().Equal(previous.ProtocolSessionID()) ||
		next.Descriptor().ShareInstance() != previous.Descriptor().ShareInstance() ||
		next.Descriptor().SyntheticRoot() != previous.Descriptor().SyntheticRoot()) {
		err = ErrReplacement
	}
	if err == nil {
		err = s.ctx.Err()
	}
	if err != nil && next != nil && next != previous {
		next.Close()
	}
	// The replacement flight includes disposal of rejected authority, so Close
	// cannot return while a failed candidate still owns transport resources.
	s.mu.Lock()
	if err == nil {
		s.current = next
	} else {
		s.failure = err
	}
	s.flight = nil
	close(flight)
	s.mu.Unlock()
}

func (s *Session) NewTransferJob(intent transfer.ReceiveIntent, id transfer.TransferJobID, materializer transfer.DirectTreeMaterializer, tracer transfer.TransferLifecycleTracer) (*transfer.TransferJob, error) {
	runtime := s.Runtime()
	if intent.IsZero() || intent.ShareInstance() != runtime.Descriptor().ShareInstance() ||
		intent.SyntheticRoot() != runtime.Descriptor().SyntheticRoot() {
		return nil, transfer.ErrInvalidTransferJob
	}
	return transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: id, Session: s, Tracer: tracer,
		Catalog: s, Revisions: s, Blocks: s, Materializer: materializer,
		RevisionWait: &revisionwait.Config{GenerationFence: s},
	})
}

func (s *Session) Current() revisionwait.GenerationToken {
	if s.ctx.Err() != nil {
		return revisionwait.GenerationToken{}
	}
	token, _ := revisionwait.GenerationTokenFromBytes(s.Runtime().ProtocolSessionID().Bytes())
	return token
}

func (s *Session) WaitForChange(ctx context.Context, expected revisionwait.GenerationToken) (revisionwait.GenerationChange, error) {
	for {
		runtime := s.Runtime()
		current := s.Current()
		if current.IsZero() {
			return revisionwait.NewGenerationEnd(expected, s.ctx.Err())
		}
		if current != expected {
			return revisionwait.NewGenerationReplacement(expected, current, nil)
		}
		select {
		case <-ctx.Done():
			return revisionwait.GenerationChange{}, ctx.Err()
		case <-s.ctx.Done():
			return revisionwait.NewGenerationEnd(expected, s.ctx.Err())
		case <-runtime.Done():
		}
		if err := s.recover(ctx, runtime); err != nil {
			return revisionwait.NewGenerationEnd(expected, err)
		}
	}
}

func (s *Session) ResolveOrdinaryOutputShape(ctx context.Context, selection transfer.SelectionSpec, budget ordinaryoutput.ShapeProbeBudget, tracer ordinaryoutput.ShapeTracer) (ordinaryoutput.ShapeDecision, error) {
	input, err := selection.OrdinaryOutputSelection()
	if err != nil {
		return ordinaryoutput.ShapeDecision{}, err
	}
	return ordinaryoutput.ResolveShape(ctx, s, input, budget, ordinaryoutput.BindShapeTracerToSession(s.Runtime().ProtocolSessionID(), tracer))
}

func (s *Session) AcquireDirectory(ctx context.Context, id catalog.DirectoryID) (catalog.DirectorySnapshot, func(), error) {
	for {
		runtime := s.Runtime()
		dependencies, err := runtime.TransferDependencies()
		if err != nil {
			return catalog.DirectorySnapshot{}, nil, err
		}
		snapshot, release, err := dependencies.AcquireDirectory(ctx, id)
		if err == nil || !runtime.AwaitPathRetirement(ctx) {
			return snapshot, release, err
		}
		if release != nil {
			release()
		}
		if err = s.recover(ctx, runtime); err != nil {
			return catalog.DirectorySnapshot{}, nil, err
		}
	}
}

func (s *Session) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return s.Runtime().ProtocolSessionID()
}
