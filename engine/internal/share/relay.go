package share

import (
	"context"
	"io"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func PlannedRelays(plan relayplan.Plan, observeFallback func(relayset.Fallback)) func(context.Context, []string, relayset.SenderFactory) (Relays, error) {
	return func(ctx context.Context, _ []string, create relayset.SenderFactory) (Relays, error) {
		value, err := relayset.NewPlannedSender(ctx, plan, create, observeFallback)
		if err != nil {
			return nil, err
		}
		return nativeRelays{value}, nil
	}
}

func prepareRelay(prepared Prepared, candidate relayprofile.Registration, d Dependencies, o *observations) (Relay, error) {
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
	endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
	if err != nil {
		return nil, err
	}
	capacity := 0
	if o.detailed {
		capacity = relayv2.DefaultLifecycleObservationCapacity
	}
	var attempts relayset.AttemptControl
	if d.Attempts != nil {
		attempts = d.Attempts(candidate.PublicURL)
	}
	config := relayset.SenderLifecycleConfig{
		Candidate: relayset.Candidate{PublicURL: candidate.PublicURL, Authorization: candidate.Authorization}, Fresh: register, ResumeToken: resumeToken,
		PrivateKey: material.SenderPrivateKey, Descriptor: material.Descriptor,
		LifecycleObservationCapacity: capacity,
		ObserveConnection:            o.attachRelay,
		Attempts:                     attempts,
		Clock:                        d.Clock,
		ObserveAttempt: func(attempt relayset.Attempt) {
			if attempt.State != relayset.AttemptParked && attempt.State != relayset.AttemptStage {
				o.emit(Observation{RelayRecovery: &RelayRecovery{Endpoint: endpoint, Attempt: attempt}})
			}
			if d.ObserveAttempt != nil {
				d.ObserveAttempt(candidate.PublicURL, attempt)
			}
		},
	}
	if route := candidate.Community; route != nil {
		config.HTTPClient, config.Origin = route.Client, route.Origin
	}
	relay, err := d.Relay(config)
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
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{})
	defer watcher.Close()
	watcher.Watch(ctx, func(networkstate.Snapshot) { wake() })
}
