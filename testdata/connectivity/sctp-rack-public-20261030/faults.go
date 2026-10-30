package main

import (
	"fmt"
	"sync"
	"time"
)

type transmission struct {
	times []time.Time
	acked bool
}
type faults struct {
	mu        sync.Mutex
	record    *recorder
	scenario  string
	drops     int
	delay     time.Duration
	armed     bool
	selected  bool
	target    uint32
	held      []byte
	chunks    map[uint32]*transmission
	witnesses int
}

func (f *faults) arm() {
	f.mu.Lock()
	f.armed = true
	f.mu.Unlock()
	f.record.event("arm", map[string]any{})
}
func (f *faults) outbound(p []byte) (bool, time.Duration, []byte, error) {
	view, err := parsePacket(p)
	if err != nil {
		return false, 0, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	drop := false
	var extra time.Duration
	var release []byte
	for _, c := range view.Data {
		tx := f.chunks[c.TSN]
		if tx == nil {
			tx = &transmission{}
			f.chunks[c.TSN] = tx
		}
		tx.times = append(tx.times, now)
		if f.armed && !f.selected {
			f.selected = true
			f.target = c.TSN
		}
		action := "pass"
		if f.selected && c.TSN == f.target {
			switch f.scenario {
			case "loss", "tail":
				if len(tx.times) <= f.drops {
					drop = true
					action = "drop"
				}
			case "reorder":
				if len(tx.times) == 1 {
					extra = 3 * f.delay
					action = "delay"
				}
			case "ambiguous":
				if len(tx.times) == 1 {
					f.held = append([]byte(nil), p...)
					drop = true
					action = "hold_original"
				}
				if len(tx.times) == 2 {
					release = f.held
					f.held = nil
					drop = true
					action = "drop_retry_release_original"
				}
			}
		}
		if f.selected && f.scenario == "ambiguous" && c.TSN == f.target+2 && len(tx.times) == 1 {
			extra = 4 * f.delay
			action = "delay_witness"
		}
		f.record.event("data_send", map[string]any{"tsn": c.TSN, "attempt": len(tx.times), "kind": c.Kind, "action": action, "extra_ms": extra.Milliseconds(), "target": f.selected && c.TSN == f.target, "bundled_data": len(view.Data)})
	}
	if drop && len(view.Data) > 1 {
		f.record.event("collateral_drop", map[string]any{"data": view.Data})
	}
	if release != nil {
		f.record.event("original_released", map[string]any{"tsn": f.target})
	}
	return drop, extra, release, nil
}
func (f *faults) inbound(p []byte) error {
	view, err := parsePacket(p)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range view.SACKs {
		f.record.event("sack_receive", map[string]any{"cumulative": s.Cumulative, "gaps": s.Gaps, "duplicates": s.Duplicates})
		for tsn, tx := range f.chunks {
			if tx.acked || !s.acknowledges(tsn) {
				continue
			}
			tx.acked = true
			if len(tx.times) > 1 {
				f.record.event("retry_acknowledged", map[string]any{"tsn": tsn, "ack_after_latest_send_ms": float64(time.Since(tx.times[len(tx.times)-1]).Microseconds()) / 1000})
			}
			if !f.selected || tsn == f.target {
				continue
			}
			target := f.chunks[f.target]
			if len(target.times) >= 2 && tx.times[len(tx.times)-1].After(target.times[1]) {
				f.witnesses++
				f.record.event("later_send_acknowledged", map[string]any{"tsn": tsn, "send_ms": f.ms(tx.times[len(tx.times)-1]), "retry_ms": f.ms(target.times[1])})
			}
		}
	}
	return nil
}
func (f *faults) ms(t time.Time) float64 { return float64(t.Sub(f.record.start).Microseconds()) / 1000 }
func (f *faults) summary() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := map[string]any{"target_tsn": f.target, "later_send_witnesses": f.witnesses, "scenario": f.scenario, "drops_requested": f.drops}
	target := f.chunks[f.target]
	if target == nil {
		return result
	}
	times := make([]float64, len(target.times))
	for i, t := range target.times {
		times[i] = f.ms(t)
	}
	result["target_send_ms"] = times
	result["target_attempts"] = len(times)
	if (f.scenario == "loss" || f.scenario == "tail") && len(times) > f.drops && f.drops > 0 {
		result["last_drop_to_recovery_ms"] = times[f.drops] - times[f.drops-1]
		result["original_to_recovery_ms"] = times[f.drops] - times[0]
	}
	if other := f.chunks[f.target+2]; other != nil {
		result["third_chunk_attempts"] = len(other.times)
	}
	t3, marks := f.record.counters(f.target)
	result["sender_t3_timeouts"], result["target_rack_marks"] = t3, marks
	_, thirdMarks := f.record.counters(f.target + 2)
	result["third_chunk_rack_marks"] = thirdMarks
	return result
}
func (f *faults) validate() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.selected {
		return fmt.Errorf("fault target was never selected")
	}
	if (f.scenario == "loss" || f.scenario == "tail") && len(f.chunks[f.target].times) <= f.drops {
		return fmt.Errorf("target never recovered")
	}
	if f.scenario == "loss" && f.drops >= 2 && f.witnesses == 0 {
		return fmt.Errorf("no acknowledged transmission sent after first retry; experiment is inconclusive")
	}
	return nil
}
