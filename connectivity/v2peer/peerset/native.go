package peerset

import (
	"context"
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
	p.bindControls()
	nextRefresh := time.Time{}
	for {
		now := p.owner.config.Clock.Now()
		if !now.Before(nextRefresh) {
			p.sendControl(ctx, protocolsession.PeerPathDemand)
			nextRefresh = now.Add(nativepeer.ControlRefresh)
		}
		timer := p.owner.config.Clock.NewTimer(nativepeer.MaintenanceInterval)
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
			native.Maintain(ctx)
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
	direct = direct || p.lane.ID != 0
	p.mu.Unlock()
	_, _ = p.config.Native.SetDemand([16]byte(p.key.session), p.key.path, p.mappingDemand(), direct)
	p.mu.Lock()
	p.networkGeneration = p.config.Native.Generation([16]byte(p.key.session), p.key.path)
	p.mu.Unlock()
}
func (p *Path) consumeRestart() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	requested := p.restartRequested
	p.restartRequested = false
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

func (p *Path) bindControls() {
	if p.config.Controls != nil {
		p.config.Controls.SetPeerPathControlHandler(func(_ context.Context, body []byte) error {
			control, err := protocolsession.DecodePeerPathControl(body)
			if err != nil {
				return nil
			}
			p.owner.mu.Lock()
			path := p.owner.paths[pathKey{p.key.session, v2signal.PeerPathID(control.PeerPathID)}]
			p.owner.mu.Unlock()
			if path != nil && path.config.Native != nil {
				path.config.Native.ApplyControl([16]byte(path.key.session), body)
			}
			return nil
		})
	}
}
func (p *Path) handleNativeChange(ctx context.Context, change nativepeer.Change) {

	p.mu.Lock()
	if change.Retired {
		p.retired = true
		p.restartRequested = true
	}
	if change.MappingReady || change.RemoteNetworkChanged {
		p.mappingPending = true
	}
	if change.NetworkGenerationID != 0 && p.networkGeneration != 0 && change.NetworkGenerationID != p.networkGeneration {
		p.restartRequested = true
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
