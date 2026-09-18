package share

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/liveshare"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func prepareRelay(prepared Prepared, relayURL string, d Dependencies, o *observations) (Relay, error) {
	material := prepared.Registration()
	shareID, shareInstance, pkHash, err := relayRegistrationIdentity(material)
	if err != nil {
		return nil, err
	}
	var resumeToken v2.ResumeToken
	if _, err := io.ReadFull(d.Control.Random, resumeToken[:]); err != nil {
		return nil, err
	}
	register, err := relayv2.NewFreshRegisterInit(shareID, shareInstance, pkHash, material.Descriptor, resumeToken)
	if err != nil {
		return nil, err
	}
	endpoint, err := v2.NormalizeRelayEndpoint(relayURL)
	if err != nil {
		return nil, err
	}
	capacity := 0
	if o.detailed {
		capacity = relayv2.DefaultLifecycleObservationCapacity
	}
	relay, err := d.Relay(senderrelay.Config{
		RelayURL: relayURL, Fresh: register, ResumeToken: resumeToken,
		PrivateKey: material.SenderPrivateKey, Descriptor: material.Descriptor,
		LifecycleObservationCapacity: capacity,
		ObserveConnection:            o.attachRelay,
		ObserveAttempt: func(attempt senderrelay.Attempt) {
			o.emit(Observation{RelayRecovery: &RelayRecovery{Endpoint: endpoint, Attempt: attempt}})
		},
	})
	if relay != nil {
		o.registerRelay(relay)
	}
	return relay, err
}

func relayRegistrationIdentity(material liveshare.RegistrationMaterial) (v2.ShareID, v2.ShareInstance, v2.PKHash, error) {
	shareID, err := v2.ShareIDFromBytes(material.ShareID)
	if err != nil {
		return v2.ShareID{}, v2.ShareInstance{}, v2.PKHash{}, err
	}
	instance, err := v2.ShareInstanceFromBytes(material.ShareInstance)
	if err != nil {
		return v2.ShareID{}, v2.ShareInstance{}, v2.PKHash{}, err
	}
	hash, err := v2.PKHashFromBytes(material.PKHash)
	return shareID, instance, hash, err
}

func wakeOnNetworkChange(ctx context.Context, wake func()) {
	monitor := networkstate.NewMonitor(nil, networkstate.DefaultDebounce)
	ticker := time.NewTicker(networkstate.DefaultPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if _, changed, err := monitor.Poll(ctx, now); err == nil && changed {
				wake()
			}
		}
	}
}
