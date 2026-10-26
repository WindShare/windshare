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
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"

	sessionobs "github.com/windshare/windshare/core/session/observation"
	sendersession "github.com/windshare/windshare/core/session/sendersession"
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
	ShareReady              Milestone = "share_ready"
	ShareStopping           Milestone = "share_stopping"
	ShareStopped            Milestone = "share_stopped"
	SessionRetired          Milestone = "session_retired"
)

type RelayRecovery struct {
	Endpoint v2.RelayEndpoint
	Attempt  senderrelay.Attempt
}

type ObservationSource string

const (
	ProtocolObservations ObservationSource = "protocol"

	NativeObservations ObservationSource = "native"

	RelayObservations ObservationSource = "relay"

	WebRTCObservations ObservationSource = "webrtc"

	SenderAttempts ObservationSource = "peer_attempt"

	PeerDiagnostics ObservationSource = "peer_diagnostic"
)

type ObservationLoss struct {
	Source  ObservationSource
	Dropped uint64
}

type Observation struct {
	Milestone         Milestone
	ShareInstance     catalog.ShareInstance
	Session           protocolsession.ProtocolSessionID
	Failure           error
	RelayRecovery     *RelayRecovery
	RelayAvailability *relayset.SenderAvailability
}

func (Observation) EngineEvent() {}

type observations struct {
	control    task.Control
	detailed   bool
	protocol   observationstream.Producer[sessionobs.ProtocolObservation]
	mu         sync.Mutex
	completers []func()
	losses     map[ObservationSource]uint64
	readers    sync.WaitGroup
}

func newObservations(control task.Control, request Request) *observations {
	control.Diagnostics = request.Diagnostics
	o := &observations{control: control, detailed: control.Diagnostics.Enabled()}
	if o.detailed {
		producer, consumer, _ := observationstream.New[sessionobs.ProtocolObservation](protocolObservationCapacity)
		o.protocol = producer
		readObservations(o, consumer, func(value sessionobs.ProtocolObservation) { o.control.Diagnostics.Record(value) })
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
	if value.Milestone != "" {
		o.control.Diagnostics.Record(value)
		return
	}
	if o.control.Emit != nil {
		o.control.Emit(value)
	}
}

func (o *observations) TraceCatalogStorage(value liveshare.CatalogStorageTrace) {
	o.control.Diagnostics.Record(value)
}
func (o *observations) TraceRootPrefetch(value liveshare.RootPrefetchTrace) {
	o.control.Diagnostics.Record(value)
}
func (o *observations) TraceRevision(value content.RevisionTrace) {
	o.control.Diagnostics.Record(value)
}
func (o *observations) ObserveSenderTerminalSend(value sessionobs.SenderTerminalSendObserved) {
	o.control.Diagnostics.Record(value)
}
func (o *observations) ObserveSenderSessionTerminated(value sessionobs.SenderSessionTerminated) {
	o.control.Diagnostics.Record(value)
}

func (o *observations) runtimeConfig(relays Relays, peers sendersession.PeerHandlerFactory) liveshare.RuntimeFactoryConfig {
	config := liveshare.RuntimeFactoryConfig{TerminalConnectivity: relays, PeerHandlers: peers, ProtocolObservations: o.protocol}
	if o.detailed {
		config.TerminalSendObserver, config.SessionTerminalObserver = o, o
	}
	return config
}

func (o *observations) attachRelay(connection senderrelay.Connection) func() {
	if !o.detailed {
		return func() {}
	}
	return readObservations(o, connection.LifecycleTrace(), func(value relayv2.LifecycleTrace) { o.control.Diagnostics.Record(value) })
}

func (o *observations) registerRelay(relay Relay) {
	o.registerCompletion(func() {
		completion := relay.CompleteObservations()
		o.loss(RelayObservations, completion.Loss.CapacityDropped)
	})
}

func (o *observations) attachPeers(peers *v2peer.Factory) {
	readObservations(o, peers.SenderAttemptObservations(), func(value v2peer.SenderAttemptObservation) { o.control.Diagnostics.Record(value) })
	readObservations(o, peers.PeerDiagnostics(), func(value v2peer.PeerDiagnosticObservation) { o.control.Diagnostics.Record(value) })
	native := peers.NativeConnectivity()
	if native != nil {
		readObservations(o, native.Observations(), func(value nativepeer.Observation) { o.control.Diagnostics.Record(value) })
		o.registerCompletion(func() { o.loss(NativeObservations, native.CompleteObservations().CapacityDropped) })
	}
	o.registerCompletion(func() {
		completion := peers.CompleteObservations()
		o.loss(SenderAttempts, completion.Attempts.Loss.CapacityDropped)
		o.loss(PeerDiagnostics, completion.Diagnostics.Loss.Total())
	})
}

func (o *observations) attachChannel(channel *wsrtc.Channel) {
	// Default transfers have no channel trace queue. Retaining their completion
	// closures would retain every retired Pion graph for the whole share.
	if !o.detailed || channel == nil {
		return
	}
	readObservations(o, channel.LifecycleTrace(), func(value wsrtc.LifecycleTrace) { o.control.Diagnostics.Record(value) })
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
	// Completion retains one aggregate loss delta per source even when its
	// individual diagnostic records could not fit in the host queue.
	for _, loss := range o.lossSnapshot() {
		o.control.Diagnostics.Lost(string(loss.Source), loss.Dropped)
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

// Readers only publish into the host's bounded trace queue. Presentation cannot hold
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

func (o *observations) TraceCapacity(value revisioncapacity.TraceEvent) {
	o.control.Diagnostics.Record(value)
}
