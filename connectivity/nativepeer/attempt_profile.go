package nativepeer

import (
	"fmt"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/transport/webrtc/provider"
	"net/netip"
	"sync"
	"time"
)

func (n *NativePeerConnectivity) selectProfileLocked(snapshot networkstate.Snapshot, path *pathResources, sequence uint64, now time.Time) icepolicy.AttemptICEProfile {
	if path.waveStarted.IsZero() || now.Sub(path.waveStarted) >= WaveLifetime {
		path.waveStarted = now
		path.endpointIDs = nil
		path.failureDomains = nil
		path.profileStage = 0
	}
	pool := icepolicy.DefaultPool()
	if n.config.Pool != nil {
		pool = *n.config.Pool
	}
	v4, v6 := false, false
	for _, a := range snapshot.LocalAddresses() {
		v4 = v4 || a.Is4()
		v6 = v6 || a.Is6()
	}
	selection := icepolicy.SelectionRequest{
		NetworkGenerationID: fmt.Sprint(snapshot.GenerationID()), WaveID: fmt.Sprint(path.waveStarted.UnixNano()),
		Sequence: sequence, Now: now, UsedEndpointIDs: path.endpointIDs,
		UsedFailureDomains: path.failureDomains, IPv4: v4, IPv6: v6,
	}
	facts := n.facts.Snapshot()
	if path.profileStage > 0 && path.primaryProfile.NetworkGenerationID() != selection.NetworkGenerationID {
		path.primaryProfile = icepolicy.RebindAttemptProfile(path.primaryProfile, selection, facts)
		path.backupProfile = icepolicy.RebindAttemptProfile(path.backupProfile, selection, facts)
		if len(path.backupProfile.URLs()) == 0 {
			path.backupProfile = path.primaryProfile
		}
	}
	if path.profileStage == 2 {
		path.lastURLs = path.backupProfile.URLs()
		return path.backupProfile
	}
	profile := icepolicy.SelectAttemptProfile(pool, selection, facts)
	if path.profileStage == 0 {
		path.primaryProfile = profile
		path.profileStage = 1
	} else {
		if len(profile.URLs()) == 0 {
			profile = path.primaryProfile
		}
		path.backupProfile = profile
		path.profileStage = 2
	}
	path.lastURLs = profile.URLs()
	path.endpointIDs = append(path.endpointIDs, profile.EndpointIDs()...)
	path.failureDomains = append(path.failureDomains, profile.FailureDomains()...)

	return profile
}
func (n *NativePeerConnectivity) attemptObserver(key pathKey, path *pathResources, profile icepolicy.AttemptICEProfile, binding v2signal.Binding, now time.Time) func(provider.Event) {
	generation := path.generation
	subject := Subject{ProtocolSessionID: key.session, PeerPathID: [16]byte(key.path), AttemptID: [16]byte(binding.AttemptID), AttemptSequence: binding.AttemptSequence, NetworkGenerationID: generation, ICEProfileID: profile.ID(), Side: n.config.Side}
	facts := n.facts
	var observationMu sync.Mutex
	produced := false
	firstDelay := time.Duration(-1)
	return func(event provider.Event) {
		observationMu.Lock()
		if event.Candidate != nil && event.Candidate.Type == "srflx" && event.Candidate.Origin == "ordinary" {
			if firstDelay < 0 {
				firstDelay = max(event.At.Sub(now), 0)
			}
			produced = true
			facts.RecordProfile(profile, produced, firstDelay)
		}
		if event.Milestone == "gathering_complete" && firstDelay < 0 {
			facts.RecordProfile(profile, false, 0)
		}
		observationMu.Unlock()
		n.recordSelectedMapping(key, path, generation, event.Pair)
		n.observeProvider(subject, event)
	}
}

func (n *NativePeerConnectivity) recordSelectedMapping(key pathKey, path *pathResources, generation uint64, pair *provider.PairFacts) {
	if pair == nil {
		return
	}
	n.mu.Lock()
	if current := n.paths[key]; current == path && current.generation == generation {
		current.mappedLocal = netip.AddrPort{}
		current.mappedProtocol = 0
		for _, fact := range n.mappingFactsLocked(current) {
			protocol := "udp"
			if fact.Endpoint.Protocol == reachability.TCP {
				protocol = "tcp"
			}
			if pair.LocalAddress == fact.External.Addr().String() && pair.LocalPort == fact.External.Port() && pair.Protocol == protocol {
				current.mappedLocal = fact.Endpoint.Local
				current.mappedProtocol = fact.Endpoint.Protocol
				break
			}
		}

	}
	n.mu.Unlock()
}

func (n *NativePeerConnectivity) BeginWave(session [16]byte, pathID v2signal.PeerPathID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.pathLocked(pathKey{session, pathID})
	if err != nil {
		return
	}
	path.waveStarted = n.config.Now()
	path.profileStage = 0
	path.endpointIDs = nil
	path.failureDomains = nil
}
