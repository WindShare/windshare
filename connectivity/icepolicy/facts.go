package icepolicy

import (
	"sync"
	"time"
)

const MaximumProfileFacts = 64

// FactStore records only current-network runtime facts. Endpoint errors must be
// explicitly attributable; profile production never implies endpoint success.
type FactStore struct {
	mu         sync.Mutex
	generation string
	endpoints  map[string]EndpointFact
	profiles   []ProfileFact
}

func NewFactStore(generation string) *FactStore {
	return &FactStore{generation: generation, endpoints: make(map[string]EndpointFact)}
}
func (s *FactStore) SetGeneration(generation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation == generation {
		return
	}
	s.generation = generation
	clear(s.endpoints)
	s.profiles = nil
}
func (s *FactStore) RecordEndpointFailure(profile AttemptICEProfile, endpointID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if profile.NetworkGenerationID() != s.generation || endpointID == "" {
		return false
	}
	for _, id := range profile.EndpointIDs() {
		if id == endpointID {
			if _, known := s.endpoints[id]; !known && len(s.endpoints) >= MaximumCatalogEndpoints {
				return false
			}
			s.endpoints[id] = EndpointFact{EndpointID: id, FailedUntil: now.Add(EndpointFailureCooldown)}
			return true
		}
	}
	return false
}
func (s *FactStore) RecordProfile(profile AttemptICEProfile, produced bool, delay time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if profile.NetworkGenerationID() != s.generation || delay < 0 {
		return
	}
	fact := ProfileFact{ProfileID: profile.ID(), ServerReflexiveProduced: produced, FirstCandidateDelay: delay, EndpointIDs: profile.EndpointIDs(), ObservedAt: now}
	for i, existing := range s.profiles {
		if existing.ProfileID == fact.ProfileID {
			s.profiles[i] = fact
			return
		}
	}
	if len(s.profiles) == MaximumProfileFacts {
		s.profiles = s.profiles[1:]
	}
	s.profiles = append(s.profiles, fact)
}
func (s *FactStore) Snapshot() GenerationFacts {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := GenerationFacts{NetworkGenerationID: s.generation, Profiles: append([]ProfileFact(nil), s.profiles...)}
	for i := range result.Profiles {
		result.Profiles[i].EndpointIDs = append([]string(nil), result.Profiles[i].EndpointIDs...)
	}
	for _, fact := range s.endpoints {
		result.Endpoints = append(result.Endpoints, fact)
	}
	return result
}
