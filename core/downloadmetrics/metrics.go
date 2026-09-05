// Package downloadmetrics accounts for unique authenticated content over one download.
package downloadmetrics

import (
	"sync"
	"time"
)

type Route uint8

const (
	Unknown Route = iota
	Direct
	TURN
	ApplicationRelay
)
const MaximumIntervals = 4096

type Snapshot struct {
	DownloadID            string
	FirstDirectElapsed    *time.Duration
	DirectBytes           uint64
	TURNBytes             uint64
	ApplicationRelayBytes uint64
	UnknownBytes          uint64
	DirectFraction        *float64
	FallbackStall         time.Duration
	Incomplete            bool
	Final                 bool
}
type interval struct{ start, end uint64 }

// Metrics outlives protocol generations. Its bounded interval ledger rejects
// duplicate/retried credit without retaining content or evicting old identities.
type Metrics struct {
	mu         sync.Mutex
	now        func() time.Time
	started    time.Time
	id         string
	first      *time.Duration
	direct     bool
	fallback   bool
	pending    uint64
	last       time.Time
	stall      time.Duration
	bytes      [ApplicationRelay + 1]uint64
	ranges     map[string][]interval
	intervals  int
	incomplete bool
	final      bool
	active     bool
}

func New(id string, now func() time.Time, directUsable bool) *Metrics {
	m := Prepare(now)
	m.Availability(directUsable)
	m.Activate(id)
	return m
}

// Prepare observes prewarming without starting the download clock.
func Prepare(now func() time.Time) *Metrics {
	if now == nil {
		now = time.Now
	}
	return &Metrics{now: now, ranges: make(map[string][]interval)}
}

// Activate is the content admission boundary, after local output preparation.
func (m *Metrics) Activate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active || m.final {
		return
	}
	m.id = id
	m.active = true
	m.started = m.now()
	m.last = m.started
	if m.direct {
		zero := time.Duration(0)
		m.first = &zero
	}
}
func (m *Metrics) tick() {
	if !m.active {
		return
	}
	now := m.now()
	if now.Before(m.last) {
		m.incomplete = true
		return
	}
	if m.pending > 0 && m.fallback && !m.direct {
		m.stall += now.Sub(m.last)
	}
	m.last = now
}

// Availability accepts only authenticated admitted direct-lane authority.
func (m *Metrics) Availability(direct bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.final {
		return
	}
	if !m.active {
		m.direct = direct
		return
	}
	m.tick()
	if m.direct && !direct {
		m.fallback = true
	}
	m.direct = direct
	if direct {
		m.fallback = false
		if m.first == nil {
			elapsed := m.last.Sub(m.started)
			m.first = &elapsed
		}
	}
}

// Pending brackets transport waits; callers must release it before yielding to
// output, user pauses, or local capacity work.
func (m *Metrics) Pending() func() {
	m.mu.Lock()
	if m.final || !m.active {
		m.mu.Unlock()
		return func() {}
	}
	m.tick()
	m.pending++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.final {
				return
			}
			m.tick()
			m.pending--
		})
	}
}
func (m *Metrics) EvidenceLost() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.final {
		m.incomplete = true
	}
}

func (m *Metrics) Delivered(revision string, start, end uint64, route Route) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.final || !m.active {
		return
	}
	m.tick()
	if revision == "" || end <= start || route > ApplicationRelay {
		m.incomplete = true
		return
	}
	// A useful fallback result ends the interrupted-delivery episode. Further
	// ordinary relay latency is not another fallback stall.
	if route != Unknown {
		m.fallback = false
	}
	old := m.ranges[revision]
	unique := end - start
	merged := interval{start, end}
	next := make([]interval, 0, len(old)+1)
	for _, part := range old {
		overlapStart, overlapEnd := max(start, part.start), min(end, part.end)
		if overlapEnd > overlapStart {
			unique -= overlapEnd - overlapStart
		}
		if part.end < merged.start || part.start > merged.end {
			next = append(next, part)
		} else {
			merged.start = min(merged.start, part.start)
			merged.end = max(merged.end, part.end)
		}
	}
	next = append(next, merged)
	count := m.intervals - len(old) + len(next)
	if count > MaximumIntervals {
		m.incomplete = true
		return
	}
	m.intervals = count
	m.ranges[revision] = next
	if ^uint64(0)-m.bytes[route] < unique {
		m.incomplete = true
		return
	}
	m.bytes[route] += unique
	if route == Unknown && unique > 0 {
		m.incomplete = true
	}
}
func (m *Metrics) Snapshot(final bool) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.final {
		m.tick()
	}
	if final {
		m.final = true
		clear(m.ranges)
		m.intervals = 0
	}
	result := Snapshot{DownloadID: m.id, DirectBytes: m.bytes[Direct], TURNBytes: m.bytes[TURN], ApplicationRelayBytes: m.bytes[ApplicationRelay], UnknownBytes: m.bytes[Unknown], FallbackStall: m.stall, Incomplete: m.incomplete, Final: m.final}
	if m.first != nil {
		value := *m.first
		result.FirstDirectElapsed = &value
	}
	total := float64(result.DirectBytes) + float64(result.TURNBytes) + float64(result.ApplicationRelayBytes) + float64(result.UnknownBytes)
	if total > 0 && !m.incomplete {
		fraction := float64(result.DirectBytes) / total
		result.DirectFraction = &fraction
	}
	return result
}
