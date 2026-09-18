package share

import (
	"context"
	"sync"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/task"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

const protocolObservationCapacity observationstream.Capacity = 256

type Milestone string

const (
	SourceAcquiring         Milestone = "source_acquiring"
	SourceAcquisitionFailed Milestone = "source_acquisition_failed"
	SourceAcquired          Milestone = "source_acquired"
	ShareActivated          Milestone = "share_activated"
	ShareStopping           Milestone = "share_stopping"
	ShareStopped            Milestone = "share_stopped"
	SessionRetired          Milestone = "session_retired"
)

type RelayRecovery struct {
	Endpoint v2.RelayEndpoint
	Attempt  senderrelay.Attempt
}

type ObservationSource uint8

const (
	ProtocolObservations ObservationSource = iota + 1
	NativeObservations
	RelayObservations
	WebRTCObservations
	SenderAttempts
	PeerDiagnostics
)

type ObservationLoss struct {
	Source  ObservationSource
	Dropped uint64
}

type Observation struct {
	Milestone         Milestone
	ShareInstance     catalog.ShareInstance
	Failure           error
	Catalog           *liveshare.CatalogStorageTrace
	Prefetch          *liveshare.RootPrefetchTrace
	Revision          *content.RevisionTrace
	RelayRecovery     *RelayRecovery
	RelayAvailability *relayset.SenderAvailability
	RelayLifecycle    *relayv2.LifecycleTrace
	SenderAttempt     *v2peer.SenderAttemptObservation
	PeerDiagnostic    *v2peer.PeerDiagnosticObservation
	Native            *nativepeer.Observation
	WebRTC            *wsrtc.LifecycleTrace
	Protocol          *sessionruntime.ProtocolObservation
	TerminalSend      *sessionruntime.SenderTerminalSendObserved
	SessionTerminal   *sessionruntime.SenderSessionTerminated
	Loss              *ObservationLoss
}

func (Observation) EngineEvent() {}

type observations struct {
	control    task.Control
	detailed   bool
	trace      bool
	protocol   observationstream.Producer[sessionruntime.ProtocolObservation]
	mu         sync.Mutex
	completers []func()
	losses     map[ObservationSource]uint64
	readers    sync.WaitGroup
}

func newObservations(control task.Control, request Request) *observations {
	o := &observations{control: control, detailed: request.Diagnostics, trace: request.TraceLifecycle}
	if request.Diagnostics {
		producer, consumer, _ := observationstream.New[sessionruntime.ProtocolObservation](protocolObservationCapacity)
		o.protocol = producer
		readObservations(o, consumer, func(value sessionruntime.ProtocolObservation) { o.emit(Observation{Protocol: &value}) })
	}
	return o
}

func (o *observations) sourceFactory(factory liveshare.FileSourceFactory) liveshare.FileSourceFactory {
	if factory == nil {
		return nil
	}
	return liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		o.emit(Observation{Milestone: SourceAcquiring, ShareInstance: source.ShareInstance})
		acquired, err := factory.OpenFileSource(ctx, source)
		milestone := SourceAcquired
		if err != nil {
			milestone = SourceAcquisitionFailed
		}
		o.emit(Observation{Milestone: milestone, ShareInstance: source.ShareInstance, Failure: err})
		return acquired, err
	})
}

func (o *observations) emit(value Observation) {
	if o.control.Emit != nil {
		o.control.Emit(value)
	}
}

func (o *observations) TraceCatalogStorage(value liveshare.CatalogStorageTrace) {
	o.emit(Observation{Catalog: &value})
}
func (o *observations) TraceRootPrefetch(value liveshare.RootPrefetchTrace) {
	o.emit(Observation{Prefetch: &value})
}
func (o *observations) TraceRevision(value content.RevisionTrace) {
	o.emit(Observation{Revision: &value})
}
func (o *observations) ObserveSenderTerminalSend(value sessionruntime.SenderTerminalSendObserved) {
	o.emit(Observation{TerminalSend: &value})
}
func (o *observations) ObserveSenderSessionTerminated(value sessionruntime.SenderSessionTerminated) {
	o.emit(Observation{SessionTerminal: &value})
}

func (o *observations) runtimeConfig(relays Relays, peers *v2peer.Factory) liveshare.RuntimeFactoryConfig {
	config := liveshare.RuntimeFactoryConfig{TerminalConnectivity: relays, PeerHandlers: peers, ProtocolObservations: o.protocol}
	if o.trace {
		config.TerminalSendObserver, config.SessionTerminalObserver = o, o
	}
	return config
}

func (o *observations) attachRelay(connection senderrelay.Connection) func() {
	if !o.detailed {
		return func() {}
	}
	return readObservations(o, connection.LifecycleTrace(), func(value relayv2.LifecycleTrace) { o.emit(Observation{RelayLifecycle: &value}) })
}

func (o *observations) registerRelay(relay Relay) {
	o.registerCompletion(func() {
		completion := relay.CompleteObservations()
		o.loss(RelayObservations, completion.Loss.CapacityDropped)
	})
}

func (o *observations) attachPeers(peers *v2peer.Factory) {
	readObservations(o, peers.SenderAttemptObservations(), func(value v2peer.SenderAttemptObservation) { o.emit(Observation{SenderAttempt: &value}) })
	readObservations(o, peers.PeerDiagnostics(), func(value v2peer.PeerDiagnosticObservation) { o.emit(Observation{PeerDiagnostic: &value}) })
	native := peers.NativeConnectivity()
	if native != nil {
		readObservations(o, native.Observations(), func(value nativepeer.Observation) { o.emit(Observation{Native: &value}) })
		o.registerCompletion(func() { o.loss(NativeObservations, native.CompleteObservations().CapacityDropped) })
	}
	o.registerCompletion(func() {
		completion := peers.CompleteObservations()
		o.loss(SenderAttempts, completion.Attempts.Loss.CapacityDropped)
		o.loss(PeerDiagnostics, completion.Diagnostics.Loss.CapacityDropped)
	})
}

func (o *observations) attachChannel(channel *wsrtc.Channel) {
	// Default transfers have no channel trace queue. Retaining their completion
	// closures would retain every retired Pion graph for the whole share.
	if !o.detailed || channel == nil {
		return
	}
	readObservations(o, channel.LifecycleTrace(), func(value wsrtc.LifecycleTrace) { o.emit(Observation{WebRTC: &value}) })
	o.registerCompletion(func() { o.loss(WebRTCObservations, channel.CompleteObservations().Loss.CapacityDropped) })
}

func (o *observations) registerCompletion(complete func()) {
	o.mu.Lock()
	o.completers = append(o.completers, complete)
	o.mu.Unlock()
}

func (o *observations) loss(source ObservationSource, count uint64) {
	if count == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.losses == nil {
		o.losses = make(map[ObservationSource]uint64)
	}
	previous := o.losses[source]
	if count > ^uint64(0)-previous {
		o.losses[source] = ^uint64(0)
	} else {
		o.losses[source] = previous + count
	}
}

func (o *observations) complete() {
	o.loss(ProtocolObservations, o.protocol.Complete().CapacityDropped)
	o.mu.Lock()
	complete := append([]func(){}, o.completers...)
	o.mu.Unlock()
	for _, finish := range complete {
		finish()
	}
	o.readers.Wait()
	// Readers may have emitted cumulative loss facts already. One aggregate cut
	// per source lets consumers account for the remaining loss exactly once.
	for _, loss := range o.lossSnapshot() {
		o.emit(Observation{Loss: &loss})
	}
}

func (o *observations) lossSnapshot() []ObservationLoss {
	o.mu.Lock()
	defer o.mu.Unlock()
	var result []ObservationLoss
	for _, source := range []ObservationSource{ProtocolObservations, NativeObservations, RelayObservations, WebRTCObservations, SenderAttempts, PeerDiagnostics} {
		if count := o.losses[source]; count > 0 {
			result = append(result, ObservationLoss{Source: source, Dropped: count})
		}
	}
	return result
}

// Readers only publish into the task's bounded queue. Presentation cannot hold
// their joins open, and the producer owner always closes the stream first.
func readObservations[T any](o *observations, stream <-chan T, emit func(T)) func() {
	if stream == nil {
		return func() {}
	}
	done := make(chan struct{})
	o.readers.Go(func() {
		defer close(done)
		for value := range stream {
			emit(value)
		}
	})
	return func() { <-done }
}
