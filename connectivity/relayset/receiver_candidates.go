package relayset

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/networkendpoint"
)

func mergeEndpoints(known, additional []string) ([]string, error) {
	seen := map[string]bool{}
	var endpoints []string
	for _, raw := range append(slices.Clone(known), additional...) {
		endpoint, err := networkendpoint.NormalizeRelay(raw)
		if err != nil {
			return nil, err
		}
		if !seen[endpoint.IdentityURL] {
			seen[endpoint.IdentityURL] = true
			endpoints = append(endpoints, raw)
		}
	}
	if err := validateReceiverEndpoints(endpoints); err != nil {
		return nil, err
	}
	return endpoints, nil
}

func validateReceiverCandidates(config ReceiverConfig) error {
	if len(config.Receiver.Capability.Relays) == 0 && config.ResolveEndpoints != nil {
		return nil
	}
	return validateReceiverEndpoints(config.Receiver.Capability.Relays)
}

func (receiver *Receiver) candidateSnapshot() ([]string, bool) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.candidates, receiver.incomplete
}

func (receiver *Receiver) expandCandidates() {
	defer receiver.workers.Done()
	additional, resolveErr := receiver.config.ResolveEndpoints(receiver.ctx)
	known := receiver.config.Receiver.Capability.Relays
	endpoints, err := mergeEndpoints(known, additional)
	if err == nil {
		receiver.mu.Lock()
		receiver.candidates = endpoints
		receiver.incomplete = resolveErr != nil
		receiver.mu.Unlock()
		var eligible []string
		eligible, err = receiver.config.recovery.endpoints(endpoints)
		for _, endpoint := range eligible {
			if slices.Contains(known, endpoint) {
				continue
			}
			receiver.mu.Lock()
			receiver.remaining++
			receiver.workers.Add(1)
			receiver.mu.Unlock()
			go receiver.run(endpoint)
		}
	}
	receiver.finishInitial("", errors.Join(resolveErr, err))
}

// Park only retries, preserving initial races and every admitted lane. Network
// queue pressure or path loss wakes fallback; idle browsing makes no retry traffic.
func (receiver *Receiver) waitForDemand(ctx context.Context) error {
	for {
		runtime := receiver.current()
		if runtime == nil || runtime.AttachedLanes() == 0 || runtime.LaneSet().SchedulingDemand().NetworkWaitingBytes > 0 {
			return nil
		}
		if runtime.Stopping() {
			return errors.New("receiver session stopped")
		}
		if err := receiver.config.Clock.Wait(ctx, receiverRetryDelay); err != nil {
			return err
		}
	}
}
