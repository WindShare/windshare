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

func retainResolvedEndpoints(ctx context.Context, config *ReceiverConfig) {
	if config.ResolveEndpoints == nil {
		return
	}
	// Each recovery wave keeps the first bounded expansion, including endpoints
	// whose first handshake failed, without scheduling another directory request.
	additional, _ := config.ResolveEndpoints(ctx)
	if expanded, err := mergeEndpoints(config.Receiver.Capability.Relays, additional); err == nil {
		config.Receiver.Capability.Relays = expanded
	}
	config.ResolveEndpoints = nil
}

func (receiver *Receiver) expandCandidates() {
	defer receiver.workers.Done()
	additional, err := receiver.config.ResolveEndpoints(receiver.ctx)
	if err == nil {
		var endpoints []string
		endpoints, err = mergeEndpoints(receiver.config.Receiver.Capability.Relays, additional)
		if err == nil {
			endpoints, err = receiver.config.recovery.endpoints(endpoints)
		}
		if err == nil {
			for _, endpoint := range endpoints {
				if slices.Contains(receiver.config.Receiver.Capability.Relays, endpoint) {
					continue
				}
				receiver.mu.Lock()
				receiver.remaining++
				receiver.workers.Add(1)
				receiver.mu.Unlock()
				go receiver.run(endpoint)
			}
		}
	}
	receiver.finishInitial("", err)
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
