package content

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

var ErrQuotaExceeded = errors.New("content resource quota exceeded")

const (
	DefaultSessionActiveLeases = 32
	DefaultShareActiveLeases   = 256
	DefaultProcessActiveLeases = 1_024

	DefaultSessionStableHandles = 32
	DefaultShareStableHandles   = 256
	DefaultProcessStableHandles = 1_024
)

type QuotaUsage struct {
	StableHandles uint64
	ActiveLeases  uint64
}

type QuotaLimits QuotaUsage

func DefaultSessionQuotaLimits() QuotaLimits {
	return QuotaLimits{StableHandles: DefaultSessionStableHandles, ActiveLeases: DefaultSessionActiveLeases}
}

func DefaultShareQuotaLimits() QuotaLimits {
	return QuotaLimits{StableHandles: DefaultShareStableHandles, ActiveLeases: DefaultShareActiveLeases}
}

func DefaultProcessQuotaLimits() QuotaLimits {
	return QuotaLimits{StableHandles: DefaultProcessStableHandles, ActiveLeases: DefaultProcessActiveLeases}
}

type QuotaSnapshot struct {
	Name   string
	Limits QuotaLimits
	Used   QuotaUsage
}

type QuotaAccount struct {
	mu     sync.Mutex
	name   string
	limits QuotaUsage
	used   QuotaUsage
}

func NewQuotaAccount(name string, limits QuotaLimits) (*QuotaAccount, error) {
	converted := QuotaUsage(limits)
	if name == "" || converted.StableHandles == 0 || converted.ActiveLeases == 0 {
		return nil, errors.New("content quota requires a name and positive limits")
	}
	return &QuotaAccount{name: name, limits: converted}, nil
}

func (a *QuotaAccount) Name() string {
	if a == nil {
		return ""
	}
	return a.name
}

func (a *QuotaAccount) Snapshot() QuotaSnapshot {
	if a == nil {
		return QuotaSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return QuotaSnapshot{Name: a.name, Limits: QuotaLimits(a.limits), Used: a.used}
}

func addQuota(left, right QuotaUsage) (QuotaUsage, bool) {
	if left.StableHandles > math.MaxUint64-right.StableHandles || left.ActiveLeases > math.MaxUint64-right.ActiveLeases {
		return QuotaUsage{}, false
	}
	return QuotaUsage{StableHandles: left.StableHandles + right.StableHandles, ActiveLeases: left.ActiveLeases + right.ActiveLeases}, true
}

func (a *QuotaAccount) reserve(usage QuotaUsage) (*quotaAccountReservation, error) {
	if a == nil {
		return nil, errors.New("content quota hierarchy contains a nil account")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next, ok := addQuota(a.used, usage)
	if !ok || next.StableHandles > a.limits.StableHandles || next.ActiveLeases > a.limits.ActiveLeases {
		return nil, fmt.Errorf("%w: %s", ErrQuotaExceeded, a.name)
	}
	a.used = next
	return &quotaAccountReservation{account: a, usage: usage}, nil
}

type quotaAccountReservation struct {
	once    sync.Once
	account *QuotaAccount
	usage   QuotaUsage
}

func (r *quotaAccountReservation) release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.account.mu.Lock()
		r.account.used.StableHandles -= r.usage.StableHandles
		r.account.used.ActiveLeases -= r.usage.ActiveLeases
		r.account.mu.Unlock()
	})
}

type QuotaHierarchy struct {
	Process *QuotaAccount
	Share   *QuotaAccount
	Session *QuotaAccount
}

type QuotaReservation struct {
	once     sync.Once
	accounts []*quotaAccountReservation
}

func ReserveQuota(hierarchy QuotaHierarchy, usage QuotaUsage) (*QuotaReservation, error) {
	return reserveQuotaAccounts([]*QuotaAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session}, usage)
}

func reserveQuotaAccounts(accounts []*QuotaAccount, usage QuotaUsage) (*QuotaReservation, error) {
	if len(accounts) == 0 {
		return nil, errors.New("content quota reservation requires at least one account")
	}
	seen := make(map[*QuotaAccount]struct{}, len(accounts))
	for _, account := range accounts {
		if account == nil {
			return nil, errors.New("content quota hierarchy requires non-nil accounts")
		}
		if _, duplicate := seen[account]; duplicate {
			return nil, errors.New("content quota hierarchy requires distinct accounts")
		}
		seen[account] = struct{}{}
	}
	reservation := &QuotaReservation{accounts: make([]*quotaAccountReservation, 0, len(accounts))}
	for _, account := range accounts {
		part, err := account.reserve(usage)
		if err != nil {
			reservation.Release()
			return nil, err
		}
		reservation.accounts = append(reservation.accounts, part)
	}
	return reservation, nil
}

func (r *QuotaReservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		for index := len(r.accounts) - 1; index >= 0; index-- {
			r.accounts[index].release()
		}
	})
}
