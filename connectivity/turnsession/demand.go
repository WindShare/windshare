package turnsession

import (
	"slices"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

const (
	largeTransfer = 4 << 20
	slowRate      = 1 << 20
	warmup        = 2 * time.Second
	stallWindow   = 3 * time.Second
	slowWindow    = 5 * time.Second
	sampleSpacing = time.Second
)

type deliverySample struct {
	at    time.Time
	bytes uint64
}

// Delivery clocks advance only while more network supply can help. A sink
// blocked on disk must not spend fallback quota even if its download is large.
type Demand struct {
	started, progress time.Time
	last              deliverySample
	window            []deliverySample
}

func (d *Demand) Observe(now time.Time, s lanes.SchedulingDemand) p.TURNMode {
	waiting := s.NetworkWaitingBytes > 0 || (s.QueuedBytes == 0 && s.InFlightBytes > 0 && s.RemainingBytes > s.InFlightBytes && s.CapacityWaitingBytes == 0)
	if !waiting {
		*d = Demand{}
		return 0
	}
	sample := deliverySample{at: now, bytes: s.UsefulBytes}
	if len(d.window) == 0 {
		d.started, d.progress, d.last = now, now, sample
		d.window = append(d.window, sample)
	} else if now.After(d.last.at) {
		if sample.bytes > d.last.bytes {
			d.progress = now
		}
		d.last = sample
		// Useful bytes arrive in whole blocks. Keep the most recent sample
		// at or before the cutoff so a single completion cannot reset the rate.
		cutoff := now.Add(-slowWindow)
		oldest := 0
		for oldest+1 < len(d.window) && !d.window[oldest+1].at.After(cutoff) {
			oldest++
		}
		d.window = slices.Delete(d.window, 0, oldest)
		if now.Sub(d.window[len(d.window)-1].at) >= sampleSpacing {
			d.window = append(d.window, sample)
		}
	}
	if s.RemainingBytes <= largeTransfer && d.last.at.Sub(d.started) <= warmup {
		return 0
	}
	oldest := d.window[0]
	elapsed := d.last.at.Sub(oldest.at)
	delivered := d.last.bytes - min(d.last.bytes, oldest.bytes)
	if d.last.at.Sub(d.progress) >= stallWindow || (elapsed >= slowWindow && float64(delivered) < slowRate*elapsed.Seconds()) {
		return p.TURNAll
	}
	return p.TURNParallel
}
