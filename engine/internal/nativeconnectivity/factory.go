// Package nativeconnectivity assembles the native peer transport used by
// application workflows. The workflow owns the returned factory's native
// connectivity and must stop it after its protocol sessions finish.
package nativeconnectivity

import (
	"io"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2peer"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

type SenderConfig struct {
	Now            func() time.Time
	Diagnostics    bool
	TraceLifecycle bool
	ObserveChannel func(*wsrtc.Channel)
}

func NewSender(config SenderConfig) (*v2peer.Factory, error) {
	return v2peer.NewFactory(senderConfig(config))
}

func senderConfig(config SenderConfig) v2peer.Config {
	nativeConfig := nativepeer.Config{Side: nativepeer.SideSender}
	if config.Diagnostics {
		nativeConfig.ObservationCapacity = nativepeer.DefaultObservationCapacity
	}
	result := v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		Native:        nativepeer.New(nativeConfig),
		Now:           config.Now,
		DataChannels:  channelAdapter(config.Diagnostics, config.ObserveChannel),
	}
	if config.Diagnostics || config.TraceLifecycle {
		result.SenderAttemptObservationCapacity = v2peer.DefaultSenderAttemptObservationCapacity
	}
	if config.Diagnostics {
		result.PeerDiagnosticObservationCapacity = v2peer.DefaultPeerDiagnosticObservationCapacity
	}
	return result
}

type ReceiverConfig struct {
	Random         io.Reader
	Native         *nativepeer.NativePeerConnectivity
	Diagnostics    bool
	ObserveChannel func(*wsrtc.Channel)
}

func NewReceiver(config ReceiverConfig) (*v2peer.ReceiverFactory, error) {
	return v2peer.NewReceiverFactory(receiverConfig(config))
}

func receiverConfig(config ReceiverConfig) v2peer.ReceiverFactoryConfig {
	if config.Native == nil {
		nativeConfig := nativepeer.Config{Side: nativepeer.SideReceiver}
		if config.Diagnostics {
			nativeConfig.ObservationCapacity = nativepeer.DefaultObservationCapacity
		}
		config.Native = nativepeer.New(nativeConfig)
	}
	result := v2peer.ReceiverFactoryConfig{
		Random:        config.Random,
		Native:        config.Native,
		Configuration: v2peer.DefaultConfiguration(),
		DataChannels:  channelAdapter(config.Diagnostics, config.ObserveChannel),
	}
	if config.Diagnostics {
		result.ReceiverTerminationObservationCapacity = v2peer.DefaultReceiverTerminationObservationCapacity
		result.PeerDiagnosticObservationCapacity = v2peer.DefaultPeerDiagnosticObservationCapacity
	}
	return result
}

func channelAdapter(diagnostics bool, observe func(*wsrtc.Channel)) v2peer.DataChannelAdapter {
	return v2peer.DataChannelAdapterFunc(func(channel *pion.DataChannel) (v2peer.PeerDataChannel, error) {
		capacity := 0
		if diagnostics {
			capacity = wsrtc.DefaultLifecycleObservationCapacity
		}
		wrapped, err := wsrtc.NewChannelWithOptions(channel, wsrtc.ChannelOptions{
			LifecycleObservationCapacity: capacity,
		})
		if err == nil && observe != nil {
			observe(wrapped)
		}
		return wrapped, err
	})
}
