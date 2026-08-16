package cli

import (
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/internal/testrun"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

type senderDataChannelAdapter struct {
	tracer wsrtc.LifecycleTracer
}

func (adapter senderDataChannelAdapter) WrapDataChannel(
	channel *pion.DataChannel,
) (v2peer.PeerDataChannel, error) {
	return wsrtc.NewChannelWithOptions(channel, wsrtc.ChannelOptions{
		LifecycleTracer: adapter.tracer,
	})
}

type senderPeerObservationRouter struct {
	command      v2peer.SenderAttemptObserver
	processTrace v2peer.SenderAttemptObserver
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
			command:      observations,
			processTrace: processTrace,
		},
		Now: now,
	}
	if observations == nil {
		return config
	}
	config.DataChannels = senderDataChannelAdapter{tracer: observations}
	config.OnError = observations.ObservePeerFallback
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
	return v2peer.NewFactory(senderPeerConfig(
		observations,
		v2peer.SenderAttemptObserverFunc(a.observeSenderPeerAttempt),
		now,
	))
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
