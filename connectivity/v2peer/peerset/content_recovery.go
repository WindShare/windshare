package peerset

import (
	"errors"

	"github.com/windshare/windshare/connectivity/nativepeer"

	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

var errContentRouteExhausted = errors.New("queued content exhausted its admitted path generation")

// Recovery is driven by failed content demand, independently of throughput
// growth. The existing path loop still owns closure, retry budget and admission.
func (s *ReceiverSession) recoverContentDemand(lanes *transferlanes.Set) {
	demand := lanes.RecoveryDemand()
	if len(demand) == 0 {
		return
	}
	s.owner.mu.Lock()
	paths := make([]*Path, 0, len(s.owner.paths))
	for _, path := range s.owner.paths {
		paths = append(paths, path)
	}
	s.owner.mu.Unlock()
	for _, path := range paths {
		for _, identity := range demand {
			path.recoverContentGeneration(identity)
		}
	}
}

func (p *Path) recoverContentGeneration(identity transferlanes.Identity) bool {
	p.mu.Lock()
	if p.ctx.Err() != nil || p.demand != ContentDemand || p.current == nil ||
		p.lane.ID != identity.ID || p.lane.Epoch != identity.Epoch || p.restartCause != nil {
		p.mu.Unlock()
		return false
	}
	p.restartCause = errContentRouteExhausted
	binding, generation := p.binding, p.networkGeneration
	_, relayed := p.config.ICE.Relay()
	p.mu.Unlock()
	p.config.Native.ObserveContentRecovery(nativepeer.Subject{
		ProtocolSessionID: p.key.session, PeerPathID: p.key.path,
		AttemptID: binding.AttemptID, AttemptSequence: binding.AttemptSequence,
		NetworkGenerationID: generation,
	}, !relayed)
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return true
}
