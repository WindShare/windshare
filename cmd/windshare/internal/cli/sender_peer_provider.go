package cli

import (
	"context"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/internal/testrun"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

type senderDataChannelAdapter struct {
	tracer   wsrtc.LifecycleTracer
	channels *webRTCObservationSet
}

func (adapter senderDataChannelAdapter) WrapDataChannel(
	channel *pion.DataChannel,
) (v2peer.PeerDataChannel, error) {
	wrapped, err := wsrtc.NewChannelWithOptions(channel, wsrtc.ChannelOptions{
		LifecycleTracer: adapter.tracer,
	})
	if err == nil {
		adapter.channels.register(wrapped)
	}
	return wrapped, err
}

// webRTCObservationSet retains only channel observation ownership. Product
// teardown remains with v2peer, while this owner proves that every terminal
// lifecycle callback has either committed or been counted before CLI finality.
type webRTCObservationSet struct {
	mu       sync.Mutex
	channels []*wsrtc.Channel
	once     sync.Once
	result   wsrtc.LifecycleObservationCompletion
}

func (set *webRTCObservationSet) register(channel *wsrtc.Channel) {
	if set == nil || channel == nil {
		return
	}
	set.mu.Lock()
	set.channels = append(set.channels, channel)
	set.mu.Unlock()
}

func (set *webRTCObservationSet) complete(ctx context.Context) wsrtc.LifecycleObservationCompletion {
	if set == nil {
		return wsrtc.LifecycleObservationCompletion{Drained: true}
	}
	set.once.Do(func() {
		set.mu.Lock()
		channels := append([]*wsrtc.Channel(nil), set.channels...)
		set.mu.Unlock()
		set.result.Drained = true
		for _, channel := range channels {
			completion := channel.CompleteObservations(ctx)
			set.result.Delivered = saturatingAdd(set.result.Delivered, completion.Delivered)
			set.result.Loss.QueueOverflow = saturatingAdd(set.result.Loss.QueueOverflow, completion.Loss.QueueOverflow)
			set.result.Loss.ObserverPanic = saturatingAdd(set.result.Loss.ObserverPanic, completion.Loss.ObserverPanic)
			set.result.Loss.CallbackTimeout = saturatingAdd(set.result.Loss.CallbackTimeout, completion.Loss.CallbackTimeout)
			set.result.Loss.Undrained = saturatingAdd(set.result.Loss.Undrained, completion.Loss.Undrained)
			set.result.Drained = set.result.Drained && completion.Drained
		}
	})
	return set.result
}

type senderPeerObservationRouter struct {
	command      v2peer.SenderAttemptObserver
	processTrace v2peer.SenderAttemptObserver
}

func (router senderPeerObservationRouter) ObserveSenderAttemptContext(
	ctx context.Context,
	observation v2peer.SenderAttemptObservation,
) {
	if ctx.Err() != nil {
		return
	}
	if contextual, ok := router.command.(v2peer.SenderAttemptContextObserver); ok {
		contextual.ObserveSenderAttemptContext(ctx, observation)
	} else if router.command != nil {
		router.command.ObserveSenderAttempt(observation)
	}
	if ctx.Err() == nil && router.processTrace != nil {
		router.processTrace.ObserveSenderAttempt(observation)
	}
}

func (router senderPeerObservationRouter) ObserveSenderAttempt(observation v2peer.SenderAttemptObservation) {
	// The user trace and private process trace have intentionally different
	// schemas and secrecy contracts, so they share the provider fact but not an
	// event model or recorder.
	if router.command != nil {
		router.command.ObserveSenderAttempt(observation)
	}
	if router.processTrace != nil {
		router.processTrace.ObserveSenderAttempt(observation)
	}
}

func senderPeerConfig(
	observations *shareObservations,
	processTrace v2peer.SenderAttemptObserver,
	now func() time.Time,
) v2peer.Config {
	config := v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		Observer: senderPeerObservationRouter{
			command:      observations.senderAttemptObserver(),
			processTrace: processTrace,
		},
		Now: now,
	}
	if observations == nil {
		return config
	}
	config.DataChannels = senderDataChannelAdapter{
		tracer: observations.webRTCTracer(), channels: observations.webRTCChannels,
	}
	if observations.detailedDiagnosticsEnabled() {
		config.DiagnosticObserver = observations
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
	factory, err := v2peer.NewFactory(senderPeerConfig(
		observations,
		v2peer.SenderAttemptObserverFunc(a.observeSenderPeerAttempt),
		now,
	))
	if err == nil {
		observations.registerPeerFactory(factory)
	}
	return factory, err
}

func (a *App) observeSenderPeerAttempt(observation v2peer.SenderAttemptObservation) {
	if observation.Stage != v2peer.SenderAttemptAdmitted || observation.Lane == nil {
		return
	}
	// SenderAttemptAdmitted is emitted only after the authenticated runtime owns
	// the attached lane, making it the sender-side counterpart to receiver Ready.
	a.recordProcessTrace(
		processTraceShareComponent,
		processTraceSenderDirectLane,
		testrun.OutcomeSucceeded,
	)
}
