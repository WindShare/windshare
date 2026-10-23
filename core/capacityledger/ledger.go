// Package capacityledger accounts for retained capacity without choosing admission,
// eviction, or scheduling policy. A domain owner serializes a ledger tree and all
// its holds with the same lock it uses for those decisions. This keeps allocation,
// cache pinning, and reclamation atomic without adding another lock to the hot path.
package capacityledger

import "fmt"

// Ledger must not be copied after first use. Related ledgers use one owner lock.
type Ledger struct {
	parent *Ledger
	limit  uint64
	used   uint64
	holds  uint64
	closed bool
}

type Snapshot struct {
	Limit  uint64
	Used   uint64
	Holds  uint64
	Closed bool
}

// UnreleasedError is a snapshot, not a revocation of live ownership. Late cleanup
// can still release its holds after admission has closed.
type UnreleasedError struct {
	Used  uint64
	Holds uint64
}

func (e *UnreleasedError) Error() string {
	return fmt.Sprintf("capacity ledger closed with %d units in %d live holds", e.Used, e.Holds)
}

func New(limit uint64) *Ledger { return &Ledger{limit: limit} }

func (l *Ledger) Child(limit uint64) *Ledger {
	return &Ledger{parent: l, limit: min(limit, l.limit)}
}

func (l *Ledger) Limit() uint64 { return l.limit }

func (l *Ledger) Snapshot() Snapshot {
	return Snapshot{Limit: l.limit, Used: l.used, Holds: l.holds, Closed: l.closed}
}

// Available includes every ancestor ceiling and admission boundary.
func (l *Ledger) Available() uint64 {
	available := l.limit - l.used
	for account := l; account != nil; account = account.parent {
		if account.closed {
			return 0
		}
		available = min(available, account.limit-account.used)
	}
	return available
}

func (l *Ledger) Reserve(units uint64) (Hold, bool) {
	if units == 0 || units > l.Available() {
		return Hold{}, false
	}
	for account := l; account != nil; account = account.parent {
		account.used += units
		account.holds++
	}
	return Hold{state: &holdState{ledger: l, units: units}}, true
}

func (l *Ledger) Owns(hold Hold) bool {
	return hold.active() && hold.state.ledger == l
}

func (l *Ledger) Close() error {
	l.closed = true
	if l.holds != 0 {
		return &UnreleasedError{Used: l.used, Holds: l.holds}
	}
	return nil
}
