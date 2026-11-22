package peerset

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (p *Path) maintainResources(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	native := p.config.Native
	if native == nil {
		return
	}
	changes, err := native.SetDemand([16]byte(p.key.session), p.key.path, p.mappingDemand(), false)
	if err != nil {
		return
	}
	defer native.RetainMaintenance()()
	nextRefresh := time.Time{}
	for {
		now := p.owner.config.Clock.Now()
		if !now.Before(nextRefresh) {
			p.sendControl(ctx, protocolsession.PeerPathDemand)
			nextRefresh = now.Add(nativepeer.ControlRefresh)
		}
		delay := max(nextRefresh.Sub(now), time.Nanosecond)
		timer := p.owner.config.Clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.resourceChanges:
			timer.Stop()
			p.sendControl(ctx, protocolsession.PeerPathDemand)
		case change := <-changes:
			timer.Stop()
			p.handleNativeChange(ctx, change)
		case <-timer.C():
		}
	}
}
func (p *Path) sendControl(ctx context.Context, kind protocolsession.PeerPathControlKind) {
	if p.config.Controls == nil || p.config.Native == nil {
		return
	}
	body, err := p.config.Native.Control([16]byte(p.key.session), p.key.path, kind, p.mappingDemand())
	if err == nil {
		_ = p.config.Controls.SendPeerPathControl(ctx, body)
	}
}
func (p *Path) resourceDemand(direct bool) {
	if p.config.Native == nil {
		return
	}
	p.mu.Lock()
	admitted := direct || p.lane.ID != 0
	p.mu.Unlock()
	if admitted {
		p.config.Native.SetAdmitted([16]byte(p.key.session), p.key.path, true)
	}
	_, _ = p.config.Native.SetDemand([16]byte(p.key.session), p.key.path, p.mappingDemand(), admitted)
	p.mu.Lock()
	p.networkGeneration = p.config.Native.Generation([16]byte(p.key.session), p.key.path)
	p.mu.Unlock()
}

var errNetworkGenerationReplaced = errors.New("network generation replaced")

func (p *Path) consumeRestart() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	requested := p.restartCause
	p.restartCause = nil
	return requested
}
func (p *Path) consumeMapping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ready := p.mappingPending
	p.mappingPending = false
	return ready
}

func (p *Path) mappingReady() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.mappingPending }
func (p *Path) mappingDemand() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.demand == ContentDemand && p.resourceActive
}
func (p *Path) setResourceActive(active bool) {
	p.mu.Lock()
	p.resourceActive = active
	p.mu.Unlock()
	p.resourceDemand(false)
	select {
	case p.resourceChanges <- struct{}{}:
	default:
	}
}

// PeerPathControl is session-scoped; bind the demuxing handler once per session
// rather than per Path so concurrent direct paths never overwrite the session
// slot or pin retired Path instances in the callback closure.
func (s *PeerSet) bindPeerPathControls(session protocolsession.ProtocolSessionID, controls PeerPathControlSession) {
	if controls == nil {
		return
	}
	controls.SetPeerPathControlHandler(func(_ context.Context, body []byte) error {
		s.applyPeerPathControl(session, body)
		return nil
	})
}

func (s *PeerSet) applyPeerPathControl(session protocolsession.ProtocolSessionID, body []byte) {
	control, err := protocolsession.DecodePeerPathControl(body)
	if err != nil {
		return
	}
	s.mu.Lock()
	path := s.paths[pathKey{session, v2signal.PeerPathID(control.PeerPathID)}]
	s.mu.Unlock()
	if path != nil && path.config.Native != nil {
		path.config.Native.ApplyControl([16]byte(path.key.session), body)
	}
}
func (p *Path) handleNativeChange(ctx context.Context, change nativepeer.Change) {

	p.mu.Lock()
	if change.Retired {
		p.retired = true
		p.restartCause = errNetworkGenerationReplaced
	}
	if change.MappingReady || change.RemoteNetworkChanged {
		p.mappingPending = true
	}
	if change.NetworkGenerationID != 0 && p.networkGeneration != 0 && change.NetworkGenerationID != p.networkGeneration {
		p.restartCause = errNetworkGenerationReplaced
	}
	if change.NetworkGenerationID != 0 {
		p.networkGeneration = change.NetworkGenerationID
	}
	p.mu.Unlock()
	if change.MappingReady && !change.Remote {
		p.sendControl(ctx, protocolsession.PeerPathMappingReady)
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
