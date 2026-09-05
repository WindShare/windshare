package nativepeer

import (
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

func (n *NativePeerConnectivity) pathEndpointsLocked(path *pathResources) []reachability.Endpoint {
	if path.lease == nil {
		return nil
	}
	var endpoints []reachability.Endpoint
	for _, local := range path.lease.Endpoints() {
		endpoints = append(endpoints, reachability.Endpoint{Generation: path.generation, Egress: n.egressLocked(local.Addr()), Local: local, Protocol: reachability.UDP})
	}
	for _, local := range path.lease.TCPEndpoints() {
		endpoints = append(endpoints, reachability.Endpoint{Generation: path.generation, Egress: n.egressLocked(local.Addr()), Local: local, Protocol: reachability.TCP})
	}
	return endpoints
}
func (n *NativePeerConnectivity) refreshDemandLocked(key pathKey, path *pathResources) {
	if n.config.Reachability == nil {
		return
	}
	for index, endpoint := range n.pathEndpointsLocked(path) {
		id := demandID(key, index)
		if !path.content {
			n.config.Reachability.Withdraw(id)
			continue
		}
		_ = n.config.Reachability.SetDemand(reachability.Demand{
			ID: id, Endpoint: endpoint, Until: n.config.Now().Add(reachability.DefaultDemandTTL), Content: true, Direct: path.direct,
			RetainLease: path.direct && path.mappedLocal == endpoint.Local && path.mappedProtocol == endpoint.Protocol,
		})
	}
}
func (n *NativePeerConnectivity) mappingFactsLocked(path *pathResources) []reachability.Fact {
	if n.config.Reachability == nil || path.lease == nil {
		return nil
	}
	var facts []reachability.Fact
	endpoints := n.pathEndpointsLocked(path)
	for _, fact := range n.config.Reachability.Facts() {
		if fact.Scope.Remote.IsValid() {
			continue
		}
		for _, endpoint := range endpoints {
			if fact.Endpoint == endpoint {
				facts = append(facts, fact)
				break
			}
		}
	}
	return facts
}
func (n *NativePeerConnectivity) mappedLocked(path *pathResources) []provider.MappedEndpoint {
	var mapped []provider.MappedEndpoint
	for _, fact := range n.mappingFactsLocked(path) {
		if fact.External == fact.Endpoint.Local {
			continue
		}
		protocol := "udp"
		if fact.Endpoint.Protocol == reachability.TCP {
			protocol = "tcp"
		}
		mapped = append(mapped, provider.MappedEndpoint{Local: fact.Endpoint.Local, External: fact.External, Protocol: protocol})
	}
	return mapped
}
func (n *NativePeerConnectivity) hasMappingLocked(path *pathResources) bool {
	for _, fact := range n.mappingFactsLocked(path) {
		if fact.GatewayID != "" {
			return true
		}
	}
	return false
}
