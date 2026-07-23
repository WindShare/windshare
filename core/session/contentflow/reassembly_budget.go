package contentflow

import (
	"errors"
	"fmt"
	"sync"
)

const (
	DefaultSessionReassemblyBytes   = uint64(64) << 20
	DefaultShareReassemblyBytes     = uint64(256) << 20
	DefaultProcessReassemblyBytes   = uint64(1) << 30
	DefaultSessionReassemblyRecords = uint32(16)
	DefaultShareReassemblyRecords   = uint32(64)
	DefaultProcessReassemblyRecords = uint32(256)
)

var ErrReassemblyBudget = errors.New("block reassembly budget exceeded")

type ReassemblyLimits struct {
	Bytes   uint64
	Records uint32
}

type ReassemblyUsage struct {
	Bytes   uint64
	Records uint32
}

type ReassemblyAccount struct {
	mu     sync.Mutex
	name   string
	limits ReassemblyLimits
	used   ReassemblyUsage
}

func NewReassemblyAccount(name string, limits ReassemblyLimits) (*ReassemblyAccount, error) {
	if name == "" || limits.Bytes == 0 || limits.Records == 0 {
		return nil, errors.New("reassembly account requires a name and positive limits")
	}
	return &ReassemblyAccount{name: name, limits: limits}, nil
}

func (a *ReassemblyAccount) Usage() ReassemblyUsage {
	if a == nil {
		return ReassemblyUsage{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.used
}

func (a *ReassemblyAccount) reserve(bytes uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if bytes > a.limits.Bytes-a.used.Bytes || a.used.Records == a.limits.Records {
		return fmt.Errorf("%w: %s", ErrReassemblyBudget, a.name)
	}
	a.used.Bytes += bytes
	a.used.Records++
	return nil
}

func (a *ReassemblyAccount) release(bytes uint64) {
	a.mu.Lock()
	a.used.Bytes -= bytes
	a.used.Records--
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

func reserveReassembly(hierarchy ReassemblyHierarchy, bytes uint64) (*reassemblyReservation, error) {
	accounts := []*ReassemblyAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session}
	seen := make(map[*ReassemblyAccount]struct{}, len(accounts))
	reservation := &reassemblyReservation{bytes: bytes, accounts: make([]*ReassemblyAccount, 0, len(accounts))}
	for _, account := range accounts {
		if account == nil {
			reservation.Release()
			return nil, errors.New("reassembly hierarchy contains a nil account")
		}
		if _, duplicate := seen[account]; duplicate {
			reservation.Release()
			return nil, errors.New("reassembly hierarchy accounts must be distinct")
		}
		seen[account] = struct{}{}
		if err := account.reserve(bytes); err != nil {
			reservation.Release()
			return nil, err
		}
		reservation.accounts = append(reservation.accounts, account)
	}
	return reservation, nil
}

func (r *reassemblyReservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		for index := len(r.accounts) - 1; index >= 0; index-- {
			r.accounts[index].release(r.bytes)
		}
	})
}
