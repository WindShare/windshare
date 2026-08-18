package cli

import (
	"context"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/internal/testrun"
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
