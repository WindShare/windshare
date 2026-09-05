package cli

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

type senderDataChannelAdapter struct {
	observations *shareObservations
}

func (adapter senderDataChannelAdapter) WrapDataChannel(
	channel *pion.DataChannel,
) (v2peer.PeerDataChannel, error) {
	capacity := 0
	if adapter.observations != nil && adapter.observations.detailedDiagnosticsEnabled() {
		capacity = wsrtc.DefaultLifecycleObservationCapacity
	}
	wrapped, err := wsrtc.NewChannelWithOptions(channel, wsrtc.ChannelOptions{
		LifecycleObservationCapacity: capacity,
	})
	if err == nil && adapter.observations != nil {
		adapter.observations.webRTCChannels.registerSender(wrapped, adapter.observations)
	}
	return wrapped, err
}

type observedWebRTCChannel struct {
	channel *wsrtc.Channel
	reader  *observationbridge.Reader[wsrtc.LifecycleTrace]
}

// webRTCObservationSet owns only CLI readers and completion cuts. Product
// teardown remains with v2peer and must precede complete.
type webRTCObservationSet struct {
	mu       sync.Mutex
	channels []observedWebRTCChannel
	once     sync.Once
	result   wsrtc.LifecycleObservationCompletion
	statuses []observationbridge.Status
}

func (set *webRTCObservationSet) registerSender(channel *wsrtc.Channel, observations *shareObservations) {
	if set == nil || channel == nil || observations == nil {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(channel.LifecycleTrace(), gate, func(ctx context.Context, value wsrtc.LifecycleTrace) {
		observations.webRTCLifecycleContext(ctx, gate, value)
	})
	set.register(channel, reader)
}

func (set *webRTCObservationSet) registerReceiver(channel *wsrtc.Channel, observation getObservation) {
	if set == nil || channel == nil {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(channel.LifecycleTrace(), gate, func(ctx context.Context, value wsrtc.LifecycleTrace) {
		observation.webRTCLifecycleContext(ctx, gate, value)
	})
	set.register(channel, reader)
}

func (set *webRTCObservationSet) register(
	channel *wsrtc.Channel,
	reader *observationbridge.Reader[wsrtc.LifecycleTrace],
) {
	set.mu.Lock()
	set.channels = append(set.channels, observedWebRTCChannel{channel: channel, reader: reader})
	set.mu.Unlock()
}

func (set *webRTCObservationSet) complete(
	ctx context.Context,
) (wsrtc.LifecycleObservationCompletion, []observationbridge.Status) {
	if set == nil {
		return wsrtc.LifecycleObservationCompletion{}, nil
	}
	set.once.Do(func() {
		set.mu.Lock()
		channels := append([]observedWebRTCChannel(nil), set.channels...)
		set.mu.Unlock()
		for _, observed := range channels {
			mergeWebRTCCompletion(&set.result, observed.channel.CompleteObservations())
			set.statuses = append(set.statuses, observed.reader.Join(ctx))
		}
	})
	return set.result, append([]observationbridge.Status(nil), set.statuses...)
}

func mergeWebRTCCompletion(
	total *wsrtc.LifecycleObservationCompletion,
	next wsrtc.LifecycleObservationCompletion,
) {
	total.Enqueued = saturatingAdd(total.Enqueued, next.Enqueued)
	total.Loss.CapacityDropped = saturatingAdd(total.Loss.CapacityDropped, next.Loss.CapacityDropped)
}

func senderPeerConfig(
	observations *shareObservations,
	processTraceEnabled bool,
	now func() time.Time,
) v2peer.Config {
	config := v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		Now:           now,
	}
	detailed := observations != nil && observations.detailedDiagnosticsEnabled()
	nativeConfig := nativepeer.Config{Side: nativepeer.SideSender}
	if detailed {
		nativeConfig.ObservationCapacity = nativepeer.DefaultObservationCapacity
	}
	config.Native = nativepeer.New(nativeConfig)
	if detailed || processTraceEnabled {
		config.SenderAttemptObservationCapacity = v2peer.DefaultSenderAttemptObservationCapacity
	}
	if detailed {
		config.PeerDiagnosticObservationCapacity = v2peer.DefaultPeerDiagnosticObservationCapacity
		config.DataChannels = senderDataChannelAdapter{observations: observations}
	}
	return config
}

func (a *App) newSenderPeerFactory(
	observations *shareObservations,
	clock commandClock,
) (*v2peer.Factory, error) {
	var now func() time.Time
	if clock != nil {
		now = clock.Now
	}
	processTraceEnabled := a != nil && a.processTrace != nil
	factory, err := v2peer.NewFactory(senderPeerConfig(observations, processTraceEnabled, now))
	if err == nil && observations != nil {
		observations.native = factory.NativeConnectivity()
		if observations.detailedDiagnosticsEnabled() {
			observations.nativeReader = startNativeObservation(observations.native, clievent.CommandShare, observations.observer)
		}
		observations.registerPeerFactory(factory, func(value v2peer.SenderAttemptObservation) {
			if processTraceEnabled {
				a.observeSenderPeerAttempt(value)
			}
		})
	}
	return factory, err
}

func (a *App) observeSenderPeerAttempt(observation v2peer.SenderAttemptObservation) {
	if observation.Stage != v2peer.SenderAttemptAdmitted || observation.Lane == nil {
		return
	}
	// SenderAttemptAdmitted is emitted only after authenticated runtime ownership,
	// making it the sender-side synchronization counterpart to receiver Ready.
	a.recordProcessTrace(
		processTraceShareComponent,
		processTraceSenderDirectLane,
		testrun.OutcomeSucceeded,
	)
}

func (a *App) newReceiverPeerStarter(
	observation getObservation,
	localStop *receiverLocalStop,
	stopAfterWave bool,
	options ...receiverPeerOptions,
) (receiverPeerStarter, error) {
	var native *nativepeer.NativePeerConnectivity
	if len(options) > 0 {
		native = options[0].native
	}
	if a.receiverPeerFactory != nil {
		starter, err := a.receiverPeerFactory()
		if err == nil {
			if completer, ok := starter.(receiverObservationCompleter); ok {
				observation.registerReceiverFactory(completer, localStop)
			}
		}
		return starter, err
	}
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{Native: native,
			Configuration: v2peer.DefaultConfiguration(),
			DataChannels: v2peer.DataChannelAdapterFunc(func(channel *pion.DataChannel) (v2peer.PeerDataChannel, error) {
				return wsrtc.NewChannelWithOptions(channel, wsrtc.ChannelOptions{})
			}),
		})
		if err != nil {
			return nil, err
		}
		return receiverPeerFactoryAdapter{factory: factory, stopAfterWave: stopAfterWave}, nil
	}
	factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{Native: native,
		Configuration: v2peer.DefaultConfiguration(),
		DataChannels: v2peer.DataChannelAdapterFunc(func(channel *pion.DataChannel) (v2peer.PeerDataChannel, error) {
			wrapped, wrapErr := wsrtc.NewChannelWithOptions(channel, wsrtc.ChannelOptions{
				LifecycleObservationCapacity: observation.webRTCObservationCapacity(),
			})
			if wrapErr == nil {
				observation.webRTCObservationSet().registerReceiver(wrapped, observation)
			}
			return wrapped, wrapErr
		}),
		ReceiverTerminationObservationCapacity: v2peer.DefaultReceiverTerminationObservationCapacity,
		PeerDiagnosticObservationCapacity:      v2peer.DefaultPeerDiagnosticObservationCapacity,
	})
	if err != nil {
		return nil, err
	}
	adapter := receiverPeerFactoryAdapter{factory: factory, stopAfterWave: stopAfterWave}
	observation.registerReceiverFactory(adapter, localStop)
	return adapter, nil
}

type nativeObservationSource interface {
	Observations() <-chan nativepeer.Observation
	CompleteObservations() observationstream.Completion
}
type nativeObservationReader struct {
	source nativeObservationSource
	reader *observationbridge.Reader[nativepeer.Observation]
}

func startNativeObservation(source nativeObservationSource, command clievent.Command, sink shareEventObserver) nativeObservationReader {
	if source == nil || sink == nil {
		return nativeObservationReader{}
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(source.Observations(), gate, func(ctx context.Context, value nativepeer.Observation) {
		event, err := projectNativeObservation(command, value)
		gate.Commit(ctx, func() bool {
			if err != nil {
				return sink.ReportObserverLoss(clievent.ObserverLossNativeConnectivity, clievent.ObserverLossEventContract, 1)
			}
			return sink.Observe(event)
		})
	})
	return nativeObservationReader{source: source, reader: reader}
}
func (reader nativeObservationReader) complete(ctx context.Context) (observationstream.Completion, observationbridge.Status) {
	if reader.source == nil {
		return observationstream.Completion{}, observationbridge.Status{Joined: true}
	}
	completion := reader.source.CompleteObservations()
	return completion, reader.reader.Join(ctx)
}
func (observation getObservation) registerNative(source nativeObservationSource) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	observation.state.native = startNativeObservation(source, clievent.CommandGet, observation.runtime)
}
func projectNativeObservation(command clievent.Command, value nativepeer.Observation) (clievent.NativeConnectivityObserved, error) {
	subject := value.Subject
	spec := clievent.NativeConnectivitySpec{Command: command, AttemptSequence: subject.AttemptSequence, NetworkGeneration: subject.NetworkGenerationID, Profile: subject.ICEProfileID, Side: subject.Side, State: "unknown"}
	spec.Session, _ = clievent.NewProtocolSessionID(subject.ProtocolSessionID[:])
	spec.Path, _ = clievent.NewPeerPathID(subject.PeerPathID[:])
	spec.Attempt, _ = clievent.NewPeerAttemptID(subject.AttemptID[:])
	if spec.Side == "" {
		spec.Side = "unknown"
	}
	count := 0
	if value.Provider != nil {
		count++
	}
	if value.Reachability != nil {
		count++
	}
	if value.Lifecycle != nil {
		count++
	}
	if value.Admission != nil {
		count++
	}
	if count != 1 {
		return clievent.NativeConnectivityObserved{}, clievent.ErrInvalidEvent
	}
	if admission := value.Admission; admission != nil {
		if admission.Active < 0 || admission.Queued < 0 {
			return clievent.NativeConnectivityObserved{}, clievent.ErrInvalidEvent
		}
		spec.Kind = "admission_" + string(admission.Kind)
		spec.At = admission.At
		spec.Admission = &clievent.NativeAdmissionFacts{Wait: admission.Wait, Active: uint64(admission.Active), Queued: uint64(admission.Queued), StartsRemaining: admission.StartsRemaining, STUNRemaining: admission.STUNRemaining, ActiveTimeRemaining: admission.ActiveTimeRemaining}
	}
	if lifecycle := value.Lifecycle; lifecycle != nil {
		spec.Kind = string(lifecycle.Kind)
		spec.At = lifecycle.At
		spec.Lifecycle = &clievent.NativeLifecycleFacts{Content: lifecycle.Content, Direct: lifecycle.Direct, PreviousGeneration: lifecycle.PreviousGeneration}
	}
	if p := value.Provider; p != nil {
		spec.Kind = p.Milestone
		spec.At = p.At
		// Raw provider state may be an error message for setup failures. Only
		// finite transport states are facts suitable for the shared event schema.
		if slices.Contains([]string{"new", "checking", "connected", "completed", "disconnected", "failed", "closed", "connecting"}, p.State) {
			spec.State = p.State
		}
		if c := p.Candidate; c != nil {
			spec.Candidate = &clievent.NativeCandidateFacts{Type: c.Type, Protocol: c.Protocol, Address: nativeObservedAddress(c.Address), Port: c.Port, Family: c.Family, Origin: c.Origin}
		}
		if pair := p.Pair; pair != nil {
			spec.Pair = &clievent.NativePairFacts{LocalType: pair.LocalType, RemoteType: pair.RemoteType, Protocol: pair.Protocol, LocalAddress: nativeObservedAddress(pair.LocalAddress), RemoteAddress: nativeObservedAddress(pair.RemoteAddress), LocalPort: pair.LocalPort, RemotePort: pair.RemotePort, PairRTT: pair.RoundTripTime}
		}
	}
	if r := value.Reachability; r != nil {
		spec.Kind = r.Kind
		protocol := "unknown"
		switch r.Endpoint.Protocol {
		case reachability.UDP:
			protocol = "udp"
		case reachability.TCP:
			protocol = "tcp"
		}
		spec.Reachability = &clievent.NativeReachabilityFacts{Local: r.Endpoint.Local, Remote: r.Scope.Remote, Protocol: protocol, Reason: nativeReachabilityReason(r.Error), ServerEpoch: r.ServerEpoch, ServerRestarted: r.ServerRestarted}
	}
	return clievent.NewNativeConnectivityObserved(spec)
}
func nativeObservedAddress(value string) string {
	if address, err := netip.ParseAddr(value); err == nil {
		return address.String()
	}
	return "unknown"
}
func nativeReachabilityReason(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, reachability.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, reachability.ErrCapacity):
		return "capacity"
	case errors.Is(err, reachability.ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, reachability.ErrClosed):
		return "closed"
	case errors.Is(err, reachability.ErrLeaseLost):
		return "lease_lost"
	default:
		return "unknown"
	}
}

type receiverRelayObservationSource interface {
	Endpoint() v2.RelayEndpoint
	LifecycleTrace() <-chan relayv2.LifecycleTrace
	CompleteObservations() relayv2.LifecycleObservationCompletion
}

type getRelayObservation struct {
	connection receiverRelayObservationSource
	reader     *observationbridge.Reader[relayv2.LifecycleTrace]
}

func (observation getObservation) reportSourceLoss(category clievent.ObserverLossCategory, count uint64) {
	if count > 0 && observation.runtime != nil {
		observation.runtime.ReportObserverLoss(category, clievent.ObserverLossStreamCapacity, count)
	}
}
func (observation getObservation) completeRelayReader(ctx context.Context, relay getRelayObservation) {
	completion := relay.connection.CompleteObservations()
	observation.reportReaderStatus(clievent.ObserverLossRelayLifecycle, relay.reader.Join(ctx))
	observation.reportSourceLoss(clievent.ObserverLossRelayLifecycle, completion.Loss.CapacityDropped)
}
func (observation getObservation) completeOldRelay(relay getRelayObservation) {
	ctx, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	defer cancel()
	observation.completeRelayReader(ctx, relay)
}
func (observation getObservation) completeOldReceiver(source receiverObservationCompleter, terminal *observationbridge.Reader[v2peer.ReceiverTerminationTrace], diagnostic *observationbridge.Reader[v2peer.PeerDiagnosticObservation]) {
	ctx, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	defer cancel()
	completion := source.CompleteObservations()
	observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, terminal.Join(ctx))
	observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, diagnostic.Join(ctx))
	observation.reportSourceLoss(clievent.ObserverLossReceiverTermination, saturatingAdd(completion.Terminations.Loss.CapacityDropped, completion.Diagnostics.Loss.CapacityDropped))
}
func (observation getObservation) completeOldLanes(lanes *transfer.LaneSet, reader *observationbridge.Reader[transfer.LaneSettlementSummary]) {
	ctx, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	defer cancel()
	completion := lanes.CompleteObservations()
	observation.reportReaderStatus(clievent.ObserverLossLaneSettlement, reader.Join(ctx))
	observation.reportSourceLoss(clievent.ObserverLossLaneSettlement, completion.Loss.CapacityDropped)
}

func (observation getObservation) registerRelayConnection(connection receiverRelayObservationSource) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(connection.LifecycleTrace(), gate, func(ctx context.Context, value relayv2.LifecycleTrace) {
		observation.relayLifecycleContext(ctx, gate, value)
	})
	observation.state.completionMu.Lock()
	if observation.state.relays == nil {
		observation.state.relays = make(map[v2.RelayIdentity]getRelayObservation)
	}
	key := connection.Endpoint().Identity
	previous := observation.state.relays[key]
	observation.state.relays[key] = getRelayObservation{connection: connection, reader: reader}
	observation.state.completionMu.Unlock()
	// Reconnect owns closure of the previous connection at this same endpoint.
	// Different endpoints remain live concurrently and retain separate readers.
	if previous.connection != nil {
		observation.completeOldRelay(previous)
	}
}

func (observation getObservation) registerReceiverFactory(
	factory receiverObservationCompleter,
	localStop *receiverLocalStop,
) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	terminationGate := &observationbridge.PublicationGate{}
	terminationReader := observationbridge.Start(
		factory.ReceiverTerminationObservations(),
		terminationGate,
		func(ctx context.Context, value v2peer.ReceiverTerminationTrace) {
			observation.receiverTerminationContext(ctx, terminationGate, value, localStop.snapshot())
		},
	)
	diagnosticGate := &observationbridge.PublicationGate{}
	diagnosticReader := observationbridge.Start(
		factory.PeerDiagnostics(),
		diagnosticGate,
		func(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
			observation.peerDiagnosticContext(ctx, diagnosticGate, value)
		},
	)
	observation.state.completionMu.Lock()
	previous, previousReader, previousDiagnostic := observation.state.receiver, observation.state.receiverReader, observation.state.receiverDiagnosticReader
	observation.state.receiver = factory
	observation.state.receiverReader = terminationReader
	observation.state.receiverDiagnosticReader = diagnosticReader
	observation.state.completionMu.Unlock()
	if previous != nil {
		observation.completeOldReceiver(previous, previousReader, previousDiagnostic)
	}
}

func (observation getObservation) registerLaneSet(lanes *transfer.LaneSet) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(
		lanes.SettlementObservations(),
		gate,
		func(ctx context.Context, value transfer.LaneSettlementSummary) {
			observation.traceLaneSettlementContext(ctx, gate, value)
		},
	)
	observation.state.completionMu.Lock()
	previous, previousReader := observation.state.lanes, observation.state.laneReader
	observation.state.lanes = lanes
	observation.state.laneReader = reader
	observation.state.completionMu.Unlock()
	if previous != nil {
		observation.completeOldLanes(previous, previousReader)
	}
}

func (observation getObservation) webRTCObservationSet() *webRTCObservationSet {
	if observation.state == nil {
		return nil
	}
	return observation.state.webRTC
}

func (observation getObservation) complete(ctx context.Context) {
	if observation.state == nil {
		return
	}
	observation.state.completeOnce.Do(func() {
		observation.state.completionMu.Lock()
		relays := observation.state.relays
		webRTC := observation.state.webRTC
		receiver := observation.state.receiver
		receiverReader := observation.state.receiverReader
		receiverDiagnosticReader := observation.state.receiverDiagnosticReader
		lanes := observation.state.lanes
		laneReader := observation.state.laneReader
		native := observation.state.native
		observation.state.completionMu.Unlock()

		completion, status := native.complete(ctx)
		observation.reportCumulativeLoss(observerLossNativeQueue, clievent.ObserverLossNativeConnectivity, clievent.ObserverLossStreamCapacity, completion.CapacityDropped)
		observation.reportReaderStatus(clievent.ObserverLossNativeConnectivity, status)
		for _, relay := range relays {
			observation.completeRelayReader(ctx, relay)
		}
		webRTCCompletion, webRTCStatuses := webRTC.complete(ctx)
		observation.reportWebRTCCompletion(webRTCCompletion)
		for _, status := range webRTCStatuses {
			observation.reportReaderStatus(clievent.ObserverLossWebRTCLifecycle, status)
		}
		if receiver != nil {
			completion := receiver.CompleteObservations()
			terminationStatus := receiverReader.Join(ctx)
			diagnosticStatus := receiverDiagnosticReader.Join(ctx)
			observation.reportSourceLoss(clievent.ObserverLossReceiverTermination, saturatingAdd(completion.Terminations.Loss.CapacityDropped, completion.Diagnostics.Loss.CapacityDropped))
			observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, terminationStatus)
			observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, diagnosticStatus)
		}
		if lanes != nil {
			completion := lanes.CompleteObservations()
			status := laneReader.Join(ctx)
			observation.reportSourceLoss(clievent.ObserverLossLaneSettlement, completion.Loss.CapacityDropped)
			observation.reportReaderStatus(clievent.ObserverLossLaneSettlement, status)
		}
	})
}
