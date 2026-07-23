// Package catalog models an incrementally materialized hierarchy of share nodes.
//
// Catalog metadata deliberately excludes sender-private source locators and file
// version candidates. A CatalogStore keeps those values beside public entries so
// callers cannot accidentally serialize filesystem authority into a receiver-facing
// page.
package catalog

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var ErrBudgetExceeded = errors.New("catalog resource budget exceeded")

const (
	DefaultSessionActiveScans = 4
	DefaultShareActiveScans   = 16
	DefaultProcessActiveScans = 64

	DefaultSessionScanWork = 1_048_576
	DefaultShareScanWork   = 4_194_304
	DefaultProcessScanWork = 16_777_216

	DefaultShareCommittedEntries   = 4_194_304
	DefaultProcessCommittedEntries = 16_777_216

	DefaultSessionCatalogMemory = uint64(16) << 20
	DefaultShareCatalogMemory   = uint64(64) << 20
	DefaultProcessCatalogMemory = uint64(512) << 20

	DefaultShareCatalogSpill   = uint64(2) << 30
	DefaultProcessCatalogSpill = uint64(16) << 30
)

type ResourceUsage struct {
	ActiveScans uint64
	ScanWork    uint64
	Entries     uint64
	MemoryBytes uint64
	SpillBytes  uint64
}

type BudgetLimits ResourceUsage

func DefaultSessionBudgetLimits() BudgetLimits {
	return BudgetLimits{
		ActiveScans: DefaultSessionActiveScans, ScanWork: DefaultSessionScanWork,
		Entries: math.MaxUint64, MemoryBytes: DefaultSessionCatalogMemory, SpillBytes: math.MaxUint64,
	}
}

func DefaultShareBudgetLimits() BudgetLimits {
	return BudgetLimits{
		ActiveScans: DefaultShareActiveScans, ScanWork: DefaultShareScanWork,
		Entries: DefaultShareCommittedEntries, MemoryBytes: DefaultShareCatalogMemory, SpillBytes: DefaultShareCatalogSpill,
	}
}

func DefaultProcessBudgetLimits() BudgetLimits {
	return BudgetLimits{
		ActiveScans: DefaultProcessActiveScans, ScanWork: DefaultProcessScanWork,
		Entries: DefaultProcessCommittedEntries, MemoryBytes: DefaultProcessCatalogMemory, SpillBytes: DefaultProcessCatalogSpill,
	}
}

type BudgetSnapshot struct {
	Name   string
	Limits BudgetLimits
	Used   ResourceUsage
}

type BudgetAccount struct {
	mu     sync.Mutex
	name   string
	limits ResourceUsage
	used   ResourceUsage
}

func NewBudgetAccount(name string, limits BudgetLimits) (*BudgetAccount, error) {
	if name == "" {
		return nil, errors.New("catalog budget account requires a name")
	}
	converted := ResourceUsage(limits)
	if converted.ActiveScans == 0 || converted.ScanWork == 0 || converted.Entries == 0 || converted.MemoryBytes == 0 || converted.SpillBytes == 0 {
		return nil, errors.New("catalog budget limits must all be positive")
	}
	return &BudgetAccount{name: name, limits: converted}, nil
}

func (a *BudgetAccount) Name() string {
	if a == nil {
		return ""
	}
	return a.name
}

func (a *BudgetAccount) Snapshot() BudgetSnapshot {
	if a == nil {
		return BudgetSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return BudgetSnapshot{Name: a.name, Limits: BudgetLimits(a.limits), Used: a.used}
}

func addUsage(left, right ResourceUsage) (ResourceUsage, bool) {
	if left.ActiveScans > math.MaxUint64-right.ActiveScans || left.ScanWork > math.MaxUint64-right.ScanWork || left.Entries > math.MaxUint64-right.Entries ||
		left.MemoryBytes > math.MaxUint64-right.MemoryBytes || left.SpillBytes > math.MaxUint64-right.SpillBytes {
		return ResourceUsage{}, false
	}
	return ResourceUsage{
		ActiveScans: left.ActiveScans + right.ActiveScans, ScanWork: left.ScanWork + right.ScanWork, Entries: left.Entries + right.Entries,
		MemoryBytes: left.MemoryBytes + right.MemoryBytes, SpillBytes: left.SpillBytes + right.SpillBytes,
	}, true
}

func usageWithin(used, limit ResourceUsage) bool {
	return used.ActiveScans <= limit.ActiveScans && used.ScanWork <= limit.ScanWork && used.Entries <= limit.Entries &&
		used.MemoryBytes <= limit.MemoryBytes && used.SpillBytes <= limit.SpillBytes
}

func (a *BudgetAccount) add(usage ResourceUsage) error {
	if a == nil {
		return errors.New("catalog budget hierarchy contains a nil account")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next, ok := addUsage(a.used, usage)
	if !ok || !usageWithin(next, a.limits) {
		return fmt.Errorf("%w: %s", ErrBudgetExceeded, a.name)
	}
	a.used = next
	return nil
}

func (a *BudgetAccount) release(usage ResourceUsage) {
	a.mu.Lock()
	a.used = subtractUsage(a.used, usage)
	a.mu.Unlock()
}

func subtractUsage(left, right ResourceUsage) ResourceUsage {
	return ResourceUsage{
		ActiveScans: left.ActiveScans - right.ActiveScans, ScanWork: left.ScanWork - right.ScanWork, Entries: left.Entries - right.Entries,
		MemoryBytes: left.MemoryBytes - right.MemoryBytes, SpillBytes: left.SpillBytes - right.SpillBytes,
	}
}

type BudgetHierarchy struct {
	Process *BudgetAccount
	Share   *BudgetAccount
	Session *BudgetAccount
}

type BudgetReservation struct {
	mu       sync.Mutex
	accounts []*BudgetAccount
	usage    ResourceUsage
	released bool
}

func (r *BudgetReservation) active() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.released
}

func ReserveHierarchy(hierarchy BudgetHierarchy, usage ResourceUsage) (*BudgetReservation, error) {
	accounts := []*BudgetAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session}
	return reserveAccounts(accounts, usage)
}

func reserveAccounts(accounts []*BudgetAccount, usage ResourceUsage) (*BudgetReservation, error) {
	if len(accounts) == 0 {
		return nil, errors.New("catalog budget reservation requires at least one account")
	}
	seen := make(map[*BudgetAccount]struct{}, len(accounts))
	for _, account := range accounts {
		if account == nil {
			return nil, errors.New("catalog budget hierarchy contains a nil account")
		}
		if _, exists := seen[account]; exists {
			return nil, errors.New("catalog budget hierarchy requires distinct accounts")
		}
		seen[account] = struct{}{}
	}
	reservation := &BudgetReservation{accounts: accounts}
	if err := reservation.Grow(usage); err != nil {
		return nil, err
	}
	return reservation, nil
}

// Grow adds a cumulative charge to one logical reservation. Scan work is
// commonly metered one entry at a time; aggregating it here avoids allocating a
// reservation object per directory entry outside the memory budget it enforces.
func (r *BudgetReservation) Grow(usage ResourceUsage) error {
	if r == nil {
		return errors.New("catalog budget reservation is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return errors.New("catalog budget reservation is already released")
	}
	next, ok := addUsage(r.usage, usage)
	if !ok {
		return ErrBudgetExceeded
	}
	for index, account := range r.accounts {
		err := account.add(usage)
		if err != nil {
			for rollback := index - 1; rollback >= 0; rollback-- {
				r.accounts[rollback].release(usage)
			}
			return err
		}
	}
	r.usage = next
	return nil
}

// Shrink releases a known subset while keeping the logical reservation live.
// External merge passes use it when an input run is deleted, so the spill
// budget measures peak bytes on disk instead of cumulative I/O.
func (r *BudgetReservation) Shrink(usage ResourceUsage) error {
	if r == nil {
		return errors.New("catalog budget reservation is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return errors.New("catalog budget reservation is already released")
	}
	if usage.ActiveScans > r.usage.ActiveScans || usage.ScanWork > r.usage.ScanWork ||
		usage.Entries > r.usage.Entries || usage.MemoryBytes > r.usage.MemoryBytes ||
		usage.SpillBytes > r.usage.SpillBytes {
		return errors.New("catalog budget shrink exceeds its reservation")
	}
	r.usage = subtractUsage(r.usage, usage)
	for index := len(r.accounts) - 1; index >= 0; index-- {
		r.accounts[index].release(usage)
	}
	return nil
}

func (r *BudgetReservation) keep(usage ResourceUsage) error {
	if r == nil {
		return errors.New("catalog budget reservation is nil")
	}
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return errors.New("catalog budget reservation is already released")
	}
	if usage.ActiveScans > r.usage.ActiveScans || usage.ScanWork > r.usage.ScanWork ||
		usage.Entries > r.usage.Entries || usage.MemoryBytes > r.usage.MemoryBytes ||
		usage.SpillBytes > r.usage.SpillBytes {
		r.mu.Unlock()
		return errors.New("catalog retained usage exceeds its staging reservation")
	}
	released := subtractUsage(r.usage, usage)
	r.usage = usage
	for index := len(r.accounts) - 1; index >= 0; index-- {
		r.accounts[index].release(released)
	}
	r.mu.Unlock()
	return nil
}

func (r *BudgetReservation) dropAccount(account *BudgetAccount) error {
	if r == nil || account == nil {
		return errors.New("catalog budget reservation or account is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return errors.New("catalog budget reservation is already released")
	}
	for index, candidate := range r.accounts {
		if candidate != account {
			continue
		}
		candidate.release(r.usage)
		r.accounts = append(r.accounts[:index], r.accounts[index+1:]...)
		return nil
	}
	return errors.New("catalog budget reservation does not contain the account")
}

func (r *BudgetReservation) Release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return
	}
	r.released = true
	usage := r.usage
	r.usage = ResourceUsage{}
	for index := len(r.accounts) - 1; index >= 0; index-- {
		r.accounts[index].release(usage)
	}
	r.mu.Unlock()
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type ScanAttemptIDGenerator interface {
	NewScanAttemptID() (ScanAttemptID, error)
}

type ScanAttemptIDGeneratorFunc func() (ScanAttemptID, error)

func (f ScanAttemptIDGeneratorFunc) NewScanAttemptID() (ScanAttemptID, error) { return f() }

type DirectoryGenerationGenerator interface {
	NewDirectoryGeneration() (DirectoryGeneration, error)
}

type DirectoryGenerationGeneratorFunc func() (DirectoryGeneration, error)

func (f DirectoryGenerationGeneratorFunc) NewDirectoryGeneration() (DirectoryGeneration, error) {
	return f()
}

type randomCatalogIdentities struct{}

func (randomCatalogIdentities) NewScanAttemptID() (ScanAttemptID, error) {
	var id ScanAttemptID
	if _, err := rand.Read(id[:]); err != nil {
		return ScanAttemptID{}, fmt.Errorf("generate catalog scan attempt identity: %w", err)
	}
	return id, nil
}

func (randomCatalogIdentities) NewDirectoryGeneration() (DirectoryGeneration, error) {
	var id DirectoryGeneration
	if _, err := rand.Read(id[:]); err != nil {
		return DirectoryGeneration{}, fmt.Errorf("generate catalog directory generation: %w", err)
	}
	return id, nil
}

type StoreConfig struct {
	ShareInstance ShareInstance
	Backend       CatalogBackend
	ProcessBudget *BudgetAccount
	ShareBudget   *BudgetAccount
	PageSealer    PageSealer
	FailureSealer FailureSealer
	SpillFactory  SpillFactory
	SortRunBytes  uint64
	Clock         Clock
	AttemptIDs    ScanAttemptIDGenerator
	Generations   DirectoryGenerationGenerator
}
