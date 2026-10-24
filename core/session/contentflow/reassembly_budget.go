package contentflow

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

const (
	DefaultSessionReassemblyBytes   = uint64(64) << 20
	DefaultShareReassemblyBytes     = uint64(256) << 20
	DefaultProcessReassemblyBytes   = uint64(1) << 30
	DefaultSessionReassemblyRecords = uint32(16)
	DefaultShareReassemblyRecords   = uint32(64)
	DefaultProcessReassemblyRecords = uint32(256)
)

var (
	ErrReassemblyBudget       = errors.New("block reassembly budget exceeded")
	ErrReassemblyClosed       = errors.New("block reassembly account is closed")
	reassemblyAccountSequence atomic.Uint64
)

type ReassemblyLimits struct {
	Bytes   uint64
	Records uint32
}

type ReassemblyUsage struct {
	Bytes   uint64
	Records uint32
}

type ReassemblyAccount struct {
	mu      sync.Mutex
	order   uint64
	name    string
	limits  ReassemblyLimits
	used    ReassemblyUsage
	closed  bool
	changed chan struct{}
}

func NewReassemblyAccount(name string, limits ReassemblyLimits) (*ReassemblyAccount, error) {
	if name == "" || limits.Bytes == 0 || limits.Records == 0 {
		return nil, errors.New("reassembly account requires a name and positive limits")
	}
	return &ReassemblyAccount{name: name, limits: limits, order: reassemblyAccountSequence.Add(1)}, nil
}

func (a *ReassemblyAccount) Usage() ReassemblyUsage {
	if a == nil {
		return ReassemblyUsage{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.used
}

func (a *ReassemblyAccount) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.wakeLocked()
	if a.used != (ReassemblyUsage{}) {
		return fmt.Errorf("reassembly account %s retained %+v", a.name, a.used)
	}
	return nil
}

func (a *ReassemblyAccount) wakeLocked() {
	if a.changed != nil {
		close(a.changed)
		a.changed = nil
	}
}

func (a *ReassemblyAccount) release(bytes uint64) {
	a.mu.Lock()
	a.used.Bytes -= bytes
	a.used.Records--
	a.wakeLocked()
	a.mu.Unlock()
}

type ReassemblyHierarchy struct {
	Process *ReassemblyAccount
	Share   *ReassemblyAccount
	Session *ReassemblyAccount
}

type reassemblyReservation struct {
	once     sync.Once
	bytes    uint64
	accounts []*ReassemblyAccount
}

// Checking and charging the complete hierarchy under one lock order avoids
// partial reservations and wake loops caused by rolling back a busy child.
func tryReserveReassembly(hierarchy ReassemblyHierarchy, bytes uint64) (*reassemblyReservation, <-chan struct{}, error) {
	accounts := []*ReassemblyAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session}
	for i, account := range accounts {
		if account == nil {
			return nil, nil, errors.New("reassembly hierarchy contains a nil account")
		}
		if slices.Contains(accounts[:i], account) {
			return nil, nil, errors.New("reassembly hierarchy accounts must be distinct")
		}
	}
	slices.SortFunc(accounts, func(a, b *ReassemblyAccount) int {
		if a.order < b.order {
			return -1
		}
		if a.order > b.order {
			return 1
		}
		return 0
	})
	for _, account := range accounts {
		account.mu.Lock()
	}
	defer func() {
		for _, account := range slices.Backward(accounts) {
			account.mu.Unlock()
		}
	}()
	for _, account := range accounts {
		if account.closed {
			return nil, nil, fmt.Errorf("%w: %s", ErrReassemblyClosed, account.name)
		}
		if bytes == 0 || bytes > account.limits.Bytes {
			return nil, nil, fmt.Errorf("%w: %s", ErrReassemblyBudget, account.name)
		}
	}
	for _, account := range accounts {
		if bytes > account.limits.Bytes-account.used.Bytes || account.used.Records == account.limits.Records {
			if account.changed == nil {
				account.changed = make(chan struct{})
			}
			return nil, account.changed, nil
		}
	}
	for _, account := range accounts {
		account.used.Bytes += bytes
		account.used.Records++
	}
	return &reassemblyReservation{bytes: bytes, accounts: accounts}, nil, nil
}

func reserveReassembly(hierarchy ReassemblyHierarchy, bytes uint64) (*reassemblyReservation, error) {
	reservation, _, err := tryReserveReassembly(hierarchy, bytes)
	if reservation == nil && err == nil {
		err = ErrReassemblyBudget
	}
	return reservation, err
}

func (r *reassemblyReservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		for _, account := range slices.Backward(r.accounts) {
			account.release(r.bytes)
		}
	})
}
