package receive

import (
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
)

func (a *runner) newReceiverPeerStarter(observation getObservation, localStop *receiverLocalStop, stopAfterWave bool, options ...receiverPeerOptions) (receiverPeerStarter, error) {
	if a.receiverPeerFactory != nil {
		starter, err := a.receiverPeerFactory()
		if err == nil {
			if source, ok := starter.(receiverObservationCompleter); ok {
				observation.registerReceiverFactory(source, localStop)
			}
		}
		return starter, err
	}
	var native *nativepeer.NativePeerConnectivity
	if len(options) > 0 {
		native = options[0].native
	}
	factory, err := nativeconnectivity.NewReceiver(nativeconnectivity.ReceiverConfig{Native: native, Random: a.control.Random, Diagnostics: observation.detailed, ObserveChannel: observation.registerWebRTC})
	if err != nil {
		return nil, err
	}
	adapter := receiverPeerFactoryAdapter{factory: factory, stopAfterWave: stopAfterWave}
	observation.registerReceiverFactory(adapter, localStop)
	return adapter, nil
}
