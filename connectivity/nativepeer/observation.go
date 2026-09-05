package nativepeer

import (
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/transport/webrtc/provider"
	"time"
)

const (
	DefaultObservationCapacity = 256
	SideSender                 = "sender"
	SideReceiver               = "receiver"
)

type Subject struct {
	ProtocolSessionID   [16]byte
	PeerPathID          [16]byte
	AttemptID           [16]byte
	AttemptSequence     uint64
	NetworkGenerationID uint64
	ICEProfileID        string
	Side                string
}

// An absent identity means unknown attribution. A path-owned gateway operation
// can outlive an attempt, so it never borrows the currently active attempt ID.
type Observation struct {
	Subject      Subject
	Provider     *provider.Event
	Reachability *reachability.Event
	Lifecycle    *LifecycleFacts
	Admission    *AdmissionFacts
}

type LifecycleKind string

const (
	DemandChanged  LifecycleKind = "demand_changed"
	NetworkChanged LifecycleKind = "network_changed"
	PathClosed     LifecycleKind = "path_closed"
)

type LifecycleFacts struct {
	Kind               LifecycleKind
	At                 time.Time
	Content            bool
	Direct             bool
	PreviousGeneration uint64
}

func (n *NativePeerConnectivity) observeLifecycleLocked(key pathKey, path *pathResources, kind LifecycleKind, previous uint64) {
	n.producer.TryPublish(Observation{Subject: Subject{ProtocolSessionID: key.session, PeerPathID: [16]byte(key.path), NetworkGenerationID: path.generation, Side: n.config.Side},
		Lifecycle: &LifecycleFacts{Kind: kind, At: n.config.Now(), Content: path.content, Direct: path.direct, PreviousGeneration: previous}})
}

func (n *NativePeerConnectivity) Observations() <-chan Observation {
	if n == nil {
		return nil
	}
	return n.observations
}
func (n *NativePeerConnectivity) CompleteObservations() observationstream.Completion {
	if n == nil {
		return observationstream.Completion{}
	}
	return n.producer.Complete()
}
func (n *NativePeerConnectivity) observeProvider(subject Subject, event provider.Event) {
	if event.Candidate != nil {
		copy := *event.Candidate
		event.Candidate = &copy
	}
	if event.Pair != nil {
		copy := *event.Pair
		event.Pair = &copy
	}
	n.producer.TryPublish(Observation{Subject: subject, Provider: &event})
}
func (n *NativePeerConnectivity) observeReachability(event reachability.Event) {
	subject := Subject{NetworkGenerationID: event.Endpoint.Generation, Side: n.config.Side}
	n.mu.Lock()
	for key, path := range n.paths {
		matched := false
		for _, endpoint := range n.pathEndpointsLocked(path) {
			if endpoint == event.Endpoint {
				matched = true
				break
			}
		}
		if matched {
			subject.ProtocolSessionID = key.session
			subject.PeerPathID = [16]byte(key.path)
			break
		}
	}
	n.mu.Unlock()
	n.producer.TryPublish(Observation{Subject: subject, Reachability: &event})
}
