package share

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/task"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const stoppedMessage = "Sender stopped"

// Run returns only after every borrower has released source and key authority.
func Run(ctx context.Context, request Request, dependencies Dependencies) task.Completion[Result] {
	d := dependencies.normalized()
	if ctx == nil || d.Controller == nil || d.Control.Now == nil || d.Control.Random == nil || d.Control.CleanupContext == nil {
		if d.Controller != nil {
			d.Controller.finish(ErrConfiguration)
		}
		return task.Completion[Result]{Settlement: task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: ErrConfiguration}}
	}
	started := d.Control.Now()
	o := newObservations(d.Control, request)
	resourceContext, cancelResources := context.WithCancel(context.WithoutCancel(ctx))
	active := &activeShare{dependencies: d, observations: o, cancelResources: cancelResources}
	class, operationErr := active.start(ctx, resourceContext, request)
	cleanupClass, cleanupErr := active.close()
	settlement := settleShareLifecycle(shareTriggerAfterServe(context.Cause(ctx), operationErr), context.Cause(ctx), operationErr, cleanupErr)
	runErr := settlement.serve.failure
	cleanupErr = settlement.stop.failure
	if runErr == nil && cleanupErr != nil {
		class = cleanupClass
	}
	outcome := task.OutcomeStopped
	if runErr != nil || cleanupErr != nil {
		outcome = task.OutcomeFailed
	} else {
		class = task.FailureNone
		if task.Reason(ctx) == task.Cancelled || task.Reason(ctx) == task.ApplicationClosed {
			outcome = task.OutcomeCancelled
		}
	}
	result := Result{Elapsed: max(d.Control.Now().Sub(started), 0), Ready: active.ready, ObservationLosses: o.lossSnapshot()}
	finishErr := errors.Join(runErr, cleanupErr)
	if finishErr == nil && !active.activated {
		finishErr = context.Cause(ctx)
	}
	d.Controller.finish(finishErr)
	return task.Completion[Result]{Value: result, Settlement: task.Settlement{
		Outcome: outcome, FailureClass: class, Err: errors.Join(runErr, cleanupErr), CleanupError: cleanupErr,
	}}
}

type activeShare struct {
	dependencies    Dependencies
	observations    *observations
	prepared        Prepared
	relays          Relays
	peers           *v2peer.Factory
	factory         SessionFactory
	server          *sessionServer
	cancelResources context.CancelFunc
	networkDone     chan struct{}
	ready           bool
	activated       bool
}

func (s *activeShare) start(ctx, resourceContext context.Context, request Request) (task.FailureClass, error) {
	d, o := s.dependencies, s.observations
	config := liveshare.SenderConfig{
		Source: o.sourceFactory(request.Source), Relays: request.RelayURLs, ChunkSize: request.ChunkSize,
		Random: d.Control.Random, Now: d.Control.Now,
		CatalogTracer: o, RootPrefetchTracer: o,
		RevisionCapacity: d.RevisionCapacity, CatalogBudget: d.CatalogBudget, CacheBudget: d.CacheBudget,
	}
	if request.Diagnostics {
		config.RevisionTracer = o
	}
	var err error
	s.prepared, err = d.Prepare(ctx, config)
	if err != nil {
		return task.FailureUsage, err
	}
	if s.prepared == nil {
		return task.FailureLocal, ErrConfiguration
	}
	if err := s.prepared.AuthorizeRegistration(); err != nil {
		return task.FailureLocal, err
	}
	s.relays, err = d.Relays(resourceContext, request.RelayURLs, func(_ context.Context, url string) (relayset.SenderEndpoint, error) {
		return prepareRelay(s.prepared, url, d, o)
	})
	if err != nil {
		return task.FailureNetwork, err
	}
	if s.relays == nil {
		return task.FailureLocal, ErrConfiguration
	}
	s.relays.ObserveAvailability(func(value relayset.SenderAvailability) { o.emit(Observation{RelayAvailability: &value}) })
	s.networkDone = make(chan struct{})
	go func() {
		defer close(s.networkDone)
		d.PollNetwork(resourceContext, s.relays.Wake)
	}()
	if err := s.relays.WaitReady(ctx); err != nil {
		return task.FailureNetwork, err
	}
	endpoint, err := v2.NormalizeRelayEndpoint(s.relays.ReadyRelayURL())
	if err != nil {
		return task.FailureLocal, err
	}
	s.peers, err = d.Peers(nativeconnectivity.SenderConfig{
		Now: d.Control.Now, Diagnostics: request.Diagnostics, TraceLifecycle: request.TraceLifecycle, ObserveChannel: o.attachChannel,
	})
	if s.peers != nil {
		o.attachPeers(s.peers)
	}
	if err != nil {
		return task.FailureLocal, err
	}
	if s.peers == nil {
		return task.FailureLocal, ErrConfiguration
	}
	s.factory, err = s.prepared.NewRuntimeFactory(o.runtimeConfig(s.relays, s.peers))
	if err != nil {
		return task.FailureLocal, err
	}
	if s.factory == nil {
		return task.FailureLocal, ErrConfiguration
	}
	s.ready = true
	d.Controller.publish(Ready{Capability: s.prepared.Capability(), SelectedRootSummary: s.prepared.SelectedRootSummary(), RelayEndpoint: endpoint})
	if err := d.Controller.activate(ctx, s.prepared.StartRootPrefetch); err != nil {
		if isCallerInterruption(ctx, err) {
			return task.FailureLocal, err
		}
		var publication *PublicationError
		if errors.As(err, &publication) && publication.Stage == PublicationEncoding {
			return task.FailureUsage, errors.Join(ErrPublication, err)
		}
		return task.FailureLocal, errors.Join(ErrPublication, err)
	}
	s.activated = true
	o.emit(Observation{Milestone: ShareActivated})
	s.server = startSessionServer(resourceContext, s.factory, s.relays, o)
	select {
	case <-ctx.Done():
		return task.FailureNone, context.Cause(ctx)
	case err := <-s.server.done:
		s.server.result, s.server.received = err, true
		return task.FailureNetwork, err
	}
}

func (s *activeShare) close() (task.FailureClass, error) {
	d, o := s.dependencies, s.observations
	var networkError, localError error
	o.emit(Observation{Milestone: ShareStopping})
	if s.factory != nil {
		cleanup, cancel := d.Control.CleanupContext()
		networkError = stopFactoryWithin(cleanup, s.factory, stoppedMessage)
		cancel()
	} else if s.relays != nil {
		s.relays.StopRecovery()
		cleanup, cancel := d.Control.CleanupContext()
		networkError = s.relays.Cleanup(cleanup)
		cancel()
	}
	if s.relays != nil {
		// SenderFactory bounds its own relay-cleanup wait. Its terminal join
		// therefore does not prove the relay set's terminal worker has joined.
		// That worker already owns a bounded cleanup context; retain its result
		// and join its actual ownership before releasing source/key authority.
		networkError = errors.Join(networkError, s.relays.Cleanup(context.Background()))
	}
	s.cancelResources()
	if s.networkDone != nil {
		<-s.networkDone
	}
	if s.server != nil {
		if !s.server.received {
			s.server.result = <-s.server.done
			settled := settleShareServe(shareShutdownCallerInterrupted, context.Canceled, s.server.result)
			networkError = errors.Join(networkError, settled.failure)
		}
		s.server.workers.Wait()
	}
	if s.peers != nil && s.peers.NativeConnectivity() != nil {
		cleanup, cancel := d.Control.CleanupContext()
		localError = s.peers.NativeConnectivity().Close(cleanup)
		cancel()
	}
	if s.prepared != nil {
		localError = errors.Join(localError, s.prepared.Close())
	}
	o.emit(Observation{Milestone: ShareStopped, Failure: errors.Join(networkError, localError)})
	o.complete()
	if networkError != nil {
		return task.FailureNetwork, errors.Join(networkError, localError)
	}
	return task.FailureLocal, localError
}

func stopFactoryWithin(ctx context.Context, factory SessionFactory, message string) error {
	err := factory.Stop(ctx, message)
	if ctx.Err() != nil {
		// Stop already force-cancels work under its own independent timeout.
		// Joining that same worker is necessary before releasing shared keys.
		err = errors.Join(err, factory.Stop(context.Background(), message))
	}
	return err
}

func isCallerInterruption(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errorTreeContainsOnly(err, func(leaf error) bool {
		return exactShareInterruption(leaf, ctx.Err()) || exactShareInterruption(leaf, context.Cause(ctx))
	})
}

type sessionServer struct {
	done     chan error
	workers  sync.WaitGroup
	result   error
	received bool
}

func startSessionServer(ctx context.Context, factory SessionFactory, relays Relays, o *observations) *sessionServer {
	server := &sessionServer{done: make(chan error, 1)}
	go func() {
		for {
			channel, err := relays.Accept(ctx)
			if err != nil {
				server.done <- err
				return
			}
			if channel == nil {
				server.done <- ErrConfiguration
				return
			}
			server.workers.Go(func() {
				admission, err := factory.AdmitChannel(ctx, channel)
				if err != nil {
					_ = channel.Close()
					return
				}
				if admission.Kind == sessionruntime.SenderChannelAttachedLane {
					return
				}
				if admission.Session == nil {
					_ = channel.Close()
					return
				}
				<-admission.Session.Done()
				admission.Session.Close()
				o.emit(Observation{Milestone: SessionRetired})
			})
		}
	}()
	return server
}
