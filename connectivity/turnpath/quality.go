package turnpath

import (
	"time"

	"github.com/windshare/windshare/core/session/turncontrol"
)

const (
	qualityHistorySize = 8
	qualityFreshness   = 3 * time.Second
	minimumQualityBusy = 2 * time.Second
)

type qualityPoint struct {
	value turncontrol.Quality
	at    time.Time
}

type qualityHistory struct {
	sequence uint64
	points   []qualityPoint
}

func (h *qualityHistory) retain(value turncontrol.Quality, now time.Time) bool {
	if value.SampleSequence <= h.sequence {
		return false
	}
	if len(h.points) != 0 {
		previous := h.points[len(h.points)-1].value
		if value.Lane != previous.Lane || value.NetworkGeneration != previous.NetworkGeneration {
			h.points = nil
		} else if value.ContentBytes < previous.ContentBytes || value.BusyMilliseconds < previous.BusyMilliseconds {
			return false
		}
	}
	h.sequence = value.SampleSequence
	if len(h.points) == qualityHistorySize {
		copy(h.points, h.points[1:])
		h.points = h.points[:qualityHistorySize-1]
	}
	h.points = append(h.points, qualityPoint{value, now})
	return true
}

type measuredQuality struct {
	latest   turncontrol.Quality
	rate     float64
	measured bool
}

func (h *qualityHistory) measure(now time.Time) measuredQuality {
	if len(h.points) == 0 {
		return measuredQuality{}
	}
	last := h.points[len(h.points)-1]
	result := measuredQuality{latest: last.value}
	if now.Sub(last.at) > qualityFreshness {
		return result
	}
	first := h.points[0]
	busy := last.value.BusyMilliseconds - first.value.BusyMilliseconds
	if busy < uint64(minimumQualityBusy/time.Millisecond) || last.at.Sub(first.at) < minimumQualityBusy {
		return result
	}
	result.rate = float64(last.value.ContentBytes-first.value.ContentBytes) * 1000 / float64(busy)
	result.measured = true
	return result
}

func (s *Sender) retainQuality(value turncontrol.Quality) {
	s.mu.Lock()
	path := s.paths[value.PathID]
	accepted := path != nil && path.grant.Authority == value.Authority &&
		value.Lane.Epoch >= path.admissionEpoch && path.quality.retain(value, s.config.Now())
	s.mu.Unlock()
	if accepted {
		s.wake()
	}
}

func (s *Sender) measureQuality(path *delegation, now time.Time) measuredQuality {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == nil {
		return measuredQuality{}
	}
	quality := path.quality.measure(now)
	if path.admission == nil || path.admission.lane != quality.latest.Lane || !path.admission.live() {
		quality.measured = false
	}
	return quality
}
