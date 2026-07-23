// Package connectionlimit bounds upgraded relay connections without importing
// the retired manifest/share admission model into the v2 production graph.
package connectionlimit

import (
	"errors"
	"sync"
)

const (
	DefaultMaximumConnections          = 4_096
	DefaultMaximumConnectionsPerSource = 128
)

var ErrConfig = errors.New("relay connection limit: invalid configuration")

type Config struct {
	MaximumConnections          int
	MaximumConnectionsPerSource int
}

type Limiter struct {
	mu        sync.Mutex
	maximum   int
	perSource int
	total     int
	sources   map[string]int
}

func New(config Config) (*Limiter, error) {
	if config.MaximumConnections <= 0 || config.MaximumConnectionsPerSource <= 0 ||
		config.MaximumConnectionsPerSource > config.MaximumConnections {
		return nil, ErrConfig
	}
	return &Limiter{
		maximum: config.MaximumConnections, perSource: config.MaximumConnectionsPerSource,
		sources: make(map[string]int),
	}, nil
}

func (limiter *Limiter) Admit(source string) (release func(), allowed bool) {
	if limiter == nil {
		return func() {}, false
	}
	if source == "" {
		source = "unknown"
	}
	limiter.mu.Lock()
	if limiter.total >= limiter.maximum || limiter.sources[source] >= limiter.perSource {
		limiter.mu.Unlock()
		return func() {}, false
	}
	limiter.total++
	limiter.sources[source]++
	limiter.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			limiter.mu.Lock()
			limiter.total--
			limiter.sources[source]--
			if limiter.sources[source] == 0 {
				delete(limiter.sources, source)
			}
			limiter.mu.Unlock()
		})
	}, true
}

type Snapshot struct {
	Connections int
	Sources     int
}

func (limiter *Limiter) Snapshot() Snapshot {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return Snapshot{Connections: limiter.total, Sources: len(limiter.sources)}
}
