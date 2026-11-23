package turnsession

import (
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
)

// Delivery clocks advance only while more network supply can help. A sink
// blocked on disk must not spend fallback quota even if its download is large.
type Demand struct {
	started, last, progress, slow time.Time
	useful                        uint64
}

func (d *Demand) Observe(now time.Time, s lanes.SchedulingDemand) p.TURNMode {
	waiting := s.NetworkWaitingBytes > 0 || (s.QueuedBytes == 0 && s.InFlightBytes > 0 && s.RemainingBytes > s.InFlightBytes && s.CapacityWaitingBytes == 0)
	if !waiting {
		*d = Demand{}
		return 0
	}
	if d.started.IsZero() {
		d.started, d.last, d.progress, d.slow = now, now, now, now
		d.useful = s.UsefulBytes
	}
	dt := now.Sub(d.last)
	if dt > 0 {
		delta := s.UsefulBytes - min(s.UsefulBytes, d.useful)
		if delta > 0 {
			d.progress = now
		}
		if float64(delta)/dt.Seconds() >= slowRate {
			d.slow = now
		}
		d.last, d.useful = now, s.UsefulBytes
	}
	if s.RemainingBytes <= largeTransfer && now.Sub(d.started) <= warmup {
		return 0
	}
	if now.Sub(d.progress) >= stallWindow || now.Sub(d.slow) >= slowWindow {
		return p.TURNAll
	}
	return p.TURNParallel
}
