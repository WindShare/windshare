package relayplan

import (
	"sync"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

// Observations retain ordering hints only, with neither a probe loop nor disk state.
type Observations struct {
	mu         sync.Mutex
	generation string
	entries    map[string]Observation
}

func (store *Observations) Record(value Observation) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.generation != value.NetworkGeneration {
		store.generation = value.NetworkGeneration
		store.entries = make(map[string]Observation)
	}
	if store.entries == nil {
		store.entries = make(map[string]Observation)
	}
	if _, known := store.entries[value.Identity]; !known && len(store.entries) >= nodedirectory.MaximumNodes {
		var oldest string
		for identity, fact := range store.entries {
			if oldest == "" || fact.At.Before(store.entries[oldest].At) {
				oldest = identity
			}
		}
		delete(store.entries, oldest)
	}
	store.entries[value.Identity] = value
}
func (store *Observations) Snapshot(generation string, now time.Time) []Observation {
	store.mu.Lock()
	defer store.mu.Unlock()
	if generation != store.generation {
		return nil
	}
	var result []Observation
	for identity, value := range store.entries {
		if now.Sub(value.At) > ObservationAge {
			delete(store.entries, identity)
		} else {
			result = append(result, value)
		}
	}
	return result
}
