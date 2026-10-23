package bufferbudget

import "github.com/windshare/windshare/core/capacityledger"

// Reservation carries the account and exact bytes that can be returned. Value
// copies share one claim, and an old copy cannot release a transferred claim.
type Reservation struct {
	budget *Budget
	hold   capacityledger.Hold
}

func (r Reservation) Bytes() uint64 {
	if r.budget == nil {
		return 0
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	return r.hold.Units()
}

func (r Reservation) Release() bool {
	if r.budget == nil {
		return false
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	released := r.hold.Release()
	if released {
		r.budget.wakeLocked()
	}
	return released
}

func (r Reservation) Transfer() (Reservation, bool) {
	if r.budget == nil {
		return Reservation{}, false
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	hold, ok := r.hold.Transfer()
	return Reservation{budget: r.budget, hold: hold}, ok
}

// Split preserves the aggregate admission: canonical data, output delivery, and
// an admitted attempt may separate only after their total capacity was reserved.
func (r Reservation) Split(bytes uint64) (Reservation, bool) {
	if r.budget == nil {
		return Reservation{}, false
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	hold, ok := r.hold.Split(bytes)
	return Reservation{budget: r.budget, hold: hold}, ok
}
