package capacityledger

import "math"

type holdState struct {
	ledger     *Ledger
	units      uint64
	generation uint64
}

// Hold is a copy-safe claim on already charged capacity. Copies share ownership;
// Transfer invalidates every old copy without briefly making capacity available.
// All methods follow the ledger owner's serialization contract.
type Hold struct {
	state      *holdState
	generation uint64
}

func (h Hold) active() bool {
	return h.state != nil && h.state.units != 0 && h.generation == h.state.generation
}

func (h Hold) Units() uint64 {
	if !h.active() {
		return 0
	}
	return h.state.units
}

func (h Hold) Release() bool {
	if !h.active() {
		return false
	}
	state := h.state
	for account := state.ledger; account != nil; account = account.parent {
		account.used -= state.units
		account.holds--
	}
	state.units = 0
	return true
}

func (h Hold) Transfer() (Hold, bool) {
	if !h.active() || h.state.generation == math.MaxUint64 {
		return Hold{}, false
	}
	h.state.generation++
	return Hold{state: h.state, generation: h.state.generation}, true
}

// Split partitions existing ownership without another admission or a release /
// reacquire gap. Splitting the whole hold consumes the source capability.
func (h Hold) Split(units uint64) (Hold, bool) {
	if !h.active() || units == 0 || units > h.state.units {
		return Hold{}, false
	}
	if units == h.state.units {
		return h.Transfer()
	}
	h.state.units -= units
	for account := h.state.ledger; account != nil; account = account.parent {
		account.holds++
	}
	return Hold{state: &holdState{ledger: h.state.ledger, units: units}}, true
}

// Grow meters a logical operation cumulatively, so a directory scan or a cache
// need not allocate an ownership record for every increment.
func (h Hold) Grow(units uint64) bool {
	if !h.active() || units > h.state.ledger.Available() {
		return false
	}
	for account := h.state.ledger; account != nil; account = account.parent {
		account.used += units
	}
	h.state.units += units
	return true
}

func (h Hold) Shrink(units uint64) bool {
	if !h.active() || units > h.state.units {
		return false
	}
	if units == h.state.units {
		return h.Release()
	}
	for account := h.state.ledger; account != nil; account = account.parent {
		account.used -= units
	}
	h.state.units -= units
	return true
}
