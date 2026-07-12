package admission

import (
	"errors"
	"math"
	"sync"
	"time"
)

const (
	DefaultMaxConnections            = 4096
	DefaultMaxConcurrentShares       = 1024
	DefaultMaxSharesPerSource        = 64
	DefaultMaxManifestBytesPerSource = 64 << 20
	DefaultMaxTotalManifestBytes     = 512 << 20
	DefaultRegisterPerSourceRate     = 0.5
	DefaultRegisterPerSourceBurst    = 4
	DefaultJoinPerSourceRate         = 2
	DefaultJoinPerSourceBurst        = 16
	DefaultJoinPerShareRate          = 1
	DefaultJoinPerShareBurst         = 8
)

// Rate describes a token bucket. Every enforced policy must have a positive
// refill rate and burst; disabling individual safety boundaries is intentionally
// not supported by the production controller.
type Rate struct {
	PerSecond float64
	Burst     int
}

// DefaultConfig provides conservative node defaults. They are deployment
// protection rather than product concurrency guarantees and remain configurable.
func DefaultConfig() Config {
	return Config{
		MaxConnections:            DefaultMaxConnections,
		MaxConcurrentShares:       DefaultMaxConcurrentShares,
		MaxSharesPerSource:        DefaultMaxSharesPerSource,
		MaxManifestBytesPerSource: DefaultMaxManifestBytesPerSource,
		MaxTotalManifestBytes:     DefaultMaxTotalManifestBytes,
		RegisterPerSource:         Rate{PerSecond: DefaultRegisterPerSourceRate, Burst: DefaultRegisterPerSourceBurst},
		JoinPerSource:             Rate{PerSecond: DefaultJoinPerSourceRate, Burst: DefaultJoinPerSourceBurst},
		JoinPerShare:              Rate{PerSecond: DefaultJoinPerShareRate, Burst: DefaultJoinPerShareBurst},
	}
}

// Clock is injected so admission tests never depend on sleeps or wall-clock
// precision.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Config struct {
	MaxConnections            int
	MaxConcurrentShares       int
	MaxSharesPerSource        int
	MaxManifestBytesPerSource int64
	MaxTotalManifestBytes     int64
	RegisterPerSource         Rate
	JoinPerSource             Rate
	JoinPerShare              Rate
	Clock                     Clock
}

// Limits is the immutable, externally meaningful part of Config. Runtime
// dependencies such as Clock are deliberately excluded from publication.
type Limits struct {
	MaxConnections            int
	MaxConcurrentShares       int
	MaxSharesPerSource        int
	MaxManifestBytesPerSource int64
	MaxTotalManifestBytes     int64
	RegisterPerSource         Rate
	JoinPerSource             Rate
	JoinPerShare              Rate
}

func (c Config) validate() error {
	switch {
	case c.MaxConnections <= 0:
		return errors.New("admission: MaxConnections must be positive")
	case c.MaxConcurrentShares <= 0:
		return errors.New("admission: MaxConcurrentShares must be positive")
	case c.MaxSharesPerSource <= 0:
		return errors.New("admission: MaxSharesPerSource must be positive")
	case c.MaxSharesPerSource > c.MaxConcurrentShares:
		return errors.New("admission: MaxSharesPerSource exceeds MaxConcurrentShares")
	case c.MaxManifestBytesPerSource <= 0:
		return errors.New("admission: MaxManifestBytesPerSource must be positive")
	case c.MaxTotalManifestBytes <= 0:
		return errors.New("admission: MaxTotalManifestBytes must be positive")
	case c.MaxManifestBytesPerSource > c.MaxTotalManifestBytes:
		return errors.New("admission: MaxManifestBytesPerSource exceeds MaxTotalManifestBytes")
	}
	for name, rate := range map[string]Rate{
		"RegisterPerSource": c.RegisterPerSource,
		"JoinPerSource":     c.JoinPerSource,
		"JoinPerShare":      c.JoinPerShare,
	} {
		if rate.PerSecond <= 0 || math.IsNaN(rate.PerSecond) || math.IsInf(rate.PerSecond, 0) || rate.Burst <= 0 {
			return errors.New("admission: " + name + " must have a finite positive rate and burst")
		}
	}
	return nil
}

// Decision identifies the boundary that rejected an attempt. The signaling
// layer owns translation to its wire-level error vocabulary.
type Decision uint8

const (
	Allowed Decision = iota
	ConnectionCapacityExceeded
	RegisterRateExceeded
	ConcurrentShareCapacityExceeded
	SourceShareCapacityExceeded
	ManifestCapacityExceeded
	SourceManifestCapacityExceeded
	JoinRateExceeded
	InvalidRetainedLease
	InvalidRegistration
)

func (d Decision) Allowed() bool { return d == Allowed }

type leaseKind uint8

const (
	connectionLease leaseKind = iota + 1
	shareLease
)

// Lease owns capacity until Release. Release is safe to call repeatedly and
// concurrently; only the first call changes controller accounting.
type Lease struct {
	controller      *Controller
	kind            leaseKind
	source          string
	manifestBytes   int64
	once            sync.Once
	released        bool // guarded by controller.mu
	pins            int  // guarded by controller.mu
	accounted       bool // guarded by controller.mu
	joinInitialized bool
	joinTokens      float64
	joinLastRefill  time.Time
}

func (l *Lease) Release() {
	if l == nil || l.controller == nil {
		return
	}
	l.once.Do(func() { l.controller.release(l) })
}

// LeasePin keeps a share's accounting live while another subsystem borrows its
// immutable manifest. Owner release still makes the share ineligible for resume;
// capacity is reclaimed only after the final borrower releases its pin.
type LeasePin struct {
	lease *Lease
	once  sync.Once
}

func (l *Lease) Pin() (*LeasePin, bool) {
	if l == nil || l.controller == nil {
		return nil, false
	}
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.kind != shareLease || l.released || !l.accounted {
		return nil, false
	}
	l.pins++
	return &LeasePin{lease: l}, true
}

func (p *LeasePin) Release() {
	if p == nil || p.lease == nil || p.lease.controller == nil {
		return
	}
	p.once.Do(func() { p.lease.controller.unpin(p.lease) })
}

type sourceUsage struct {
	shares               int
	pendingShares        int
	manifestBytes        int64
	pendingManifestBytes int64
}

// Snapshot is a stable accounting view for health instrumentation and tests.
type Snapshot struct {
	Connections          int
	Shares               int
	PendingShares        int
	ManifestBytes        int64
	PendingManifestBytes int64
	Sources              int
}

// Controller is a single-node admission policy. One mutex makes capacity
// checks and reservations atomic, preventing two individually valid registers
// from jointly exceeding a limit.
type Controller struct {
	cfg Config

	mu                   sync.Mutex
	connections          int
	shares               int
	pendingShares        int
	manifestBytes        int64
	pendingManifestBytes int64
	sources              map[string]sourceUsage
	register             *bucketClass
	joinSource           *bucketClass
}

func NewController(cfg Config) (*Controller, error) {
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Controller{
		cfg:        cfg,
		sources:    make(map[string]sourceUsage),
		register:   newBucketClass(cfg.RegisterPerSource),
		joinSource: newBucketClass(cfg.JoinPerSource),
	}, nil
}

func (c *Controller) Limits() Limits {
	return Limits{
		MaxConnections:            c.cfg.MaxConnections,
		MaxConcurrentShares:       c.cfg.MaxConcurrentShares,
		MaxSharesPerSource:        c.cfg.MaxSharesPerSource,
		MaxManifestBytesPerSource: c.cfg.MaxManifestBytesPerSource,
		MaxTotalManifestBytes:     c.cfg.MaxTotalManifestBytes,
		RegisterPerSource:         c.cfg.RegisterPerSource,
		JoinPerSource:             c.cfg.JoinPerSource,
		JoinPerShare:              c.cfg.JoinPerShare,
	}
}

// AdmitConnection reserves one upgraded WebSocket slot.
func (c *Controller) AdmitConnection(source string) (*Lease, Decision) {
	source = sourceKey(source)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connections >= c.cfg.MaxConnections {
		return nil, ConnectionCapacityExceeded
	}
	c.connections++
	return &Lease{controller: c, kind: connectionLease, source: source, accounted: true}, Allowed
}

// Registration owns the provisional resources of one register handshake.
// Signaling reserves the share slot before accepting a manifest and grows the
// manifest reservation as bytes arrive, so rejected or slow uploads cannot
// allocate outside admission accounting. Release rolls back any uncommitted
// resources and is safe to call repeatedly or concurrently.
type Registration struct {
	controller    *Controller
	source        string
	shareReserved bool
	manifestBytes int64
	done          bool // guarded by controller.mu
}

// BeginRegister charges the attempt-rate boundary before the peer is allowed
// to upload a potentially large manifest.
func (c *Controller) BeginRegister(source string) (*Registration, Decision) {
	source = sourceKey(source)
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.cfg.Clock.Now()
	register, ok := c.register.bucket(source, now)
	if !ok || register.tokens < 1 {
		return nil, RegisterRateExceeded
	}
	register.tokens--
	return &Registration{controller: c, source: source}, Allowed
}

// ReserveShare reserves concurrency before the manifest upload begins. A role
// timeout bounds how long a slow peer may retain this provisional capacity.
func (r *Registration) ReserveShare() Decision {
	if r == nil || r.controller == nil {
		return InvalidRegistration
	}
	c := r.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.done || r.shareReserved {
		return InvalidRegistration
	}
	usage := c.sources[r.source]
	if c.pendingShares >= c.cfg.MaxConcurrentShares-c.shares {
		return ConcurrentShareCapacityExceeded
	}
	if usage.pendingShares >= c.cfg.MaxSharesPerSource-usage.shares {
		return SourceShareCapacityExceeded
	}
	r.shareReserved = true
	c.pendingShares++
	usage.pendingShares++
	c.sources[r.source] = usage
	return Allowed
}

// ReserveManifestBytes atomically charges the next upload chunk. The caller
// must reserve before retaining the bytes in memory.
func (r *Registration) ReserveManifestBytes(bytes int64) Decision {
	if r == nil || r.controller == nil {
		return InvalidRegistration
	}
	if bytes < 0 {
		return ManifestCapacityExceeded
	}
	c := r.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.done || !r.shareReserved {
		return InvalidRegistration
	}
	if bytes == 0 {
		return Allowed
	}
	usage := c.sources[r.source]
	if bytes > c.cfg.MaxTotalManifestBytes-c.manifestBytes-c.pendingManifestBytes {
		return ManifestCapacityExceeded
	}
	if bytes > c.cfg.MaxManifestBytesPerSource-usage.manifestBytes-usage.pendingManifestBytes {
		return SourceManifestCapacityExceeded
	}
	r.manifestBytes += bytes
	c.pendingManifestBytes += bytes
	usage.pendingManifestBytes += bytes
	c.sources[r.source] = usage
	return Allowed
}

// Commit converts provisional resources into a live share lease atomically.
// A reconnect supplies the retained lease; its temporary reservations are
// rolled back while the original lease continues to own the share.
func (r *Registration) Commit(retained *Lease) (*Lease, Decision) {
	if r == nil || r.controller == nil {
		return nil, InvalidRegistration
	}
	c := r.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.done {
		return nil, InvalidRegistration
	}

	if retained != nil {
		if retained.controller != c || retained.kind != shareLease || retained.released {
			return nil, InvalidRetainedLease
		}
		c.releaseRegistrationLocked(r)
		return retained, Allowed
	}
	if !r.shareReserved {
		return nil, InvalidRegistration
	}

	usage := c.sources[r.source]
	lease := &Lease{controller: c, kind: shareLease, source: r.source, manifestBytes: r.manifestBytes, accounted: true}
	c.pendingShares--
	c.shares++
	c.pendingManifestBytes -= r.manifestBytes
	c.manifestBytes += r.manifestBytes
	usage.pendingShares--
	usage.shares++
	usage.pendingManifestBytes -= r.manifestBytes
	usage.manifestBytes += r.manifestBytes
	c.sources[r.source] = usage
	r.done = true
	return lease, Allowed
}

// Release rolls back an incomplete register handshake exactly once.
func (r *Registration) Release() {
	if r == nil || r.controller == nil {
		return
	}
	c := r.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.done {
		return
	}
	c.releaseRegistrationLocked(r)
}

func (c *Controller) releaseRegistrationLocked(r *Registration) {
	usage := c.sources[r.source]
	if r.shareReserved {
		c.pendingShares--
		usage.pendingShares--
	}
	c.pendingManifestBytes -= r.manifestBytes
	usage.pendingManifestBytes -= r.manifestBytes
	r.done = true
	if usage.shares == 0 && usage.pendingShares == 0 && usage.manifestBytes == 0 && usage.pendingManifestBytes == 0 {
		delete(c.sources, r.source)
	} else {
		c.sources[r.source] = usage
	}
}

// AdmitRegister is the atomic convenience form for callers that already own
// the manifest bytes. Signaling uses the staged Registration API so admission
// precedes network allocation.
func (c *Controller) AdmitRegister(source string, manifestBytes int64, retained *Lease) (*Lease, Decision) {
	r, decision := c.BeginRegister(source)
	if !decision.Allowed() {
		return nil, decision
	}
	defer r.Release()
	if manifestBytes < 0 {
		return nil, ManifestCapacityExceeded
	}
	if retained == nil {
		if decision = r.ReserveShare(); !decision.Allowed() {
			return nil, decision
		}
		if decision = r.ReserveManifestBytes(manifestBytes); !decision.Allowed() {
			return nil, decision
		}
	}
	return r.Commit(retained)
}

// JoinAttempt proves that one source token was consumed before signaling looks
// up an attacker-chosen share ID. The share boundary can then be evaluated only
// against a real admitted share lease, so nonexistent-key churn allocates no
// per-share limiter state.
type JoinAttempt struct {
	controller *Controller
	used       bool // guarded by controller.mu
}

func (c *Controller) BeginJoin(source string) (*JoinAttempt, Decision) {
	source = sourceKey(source)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Clock.Now()
	bySource, sourceOK := c.joinSource.bucket(source, now)
	if !sourceOK || bySource.tokens < 1 {
		return nil, JoinRateExceeded
	}
	bySource.tokens--
	return &JoinAttempt{controller: c}, Allowed
}

func (a *JoinAttempt) AllowShare(lease *Lease) Decision {
	if a == nil || a.controller == nil {
		return InvalidRegistration
	}
	c := a.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.used {
		return InvalidRegistration
	}
	a.used = true
	if lease == nil || lease.controller != c || lease.kind != shareLease || lease.released || !lease.accounted {
		return InvalidRetainedLease
	}
	now := c.cfg.Clock.Now()
	if !lease.joinInitialized {
		lease.joinInitialized = true
		lease.joinTokens = float64(c.cfg.JoinPerShare.Burst)
		lease.joinLastRefill = now
	} else if elapsed := now.Sub(lease.joinLastRefill).Seconds(); elapsed > 0 {
		lease.joinTokens = min(float64(c.cfg.JoinPerShare.Burst), lease.joinTokens+elapsed*c.cfg.JoinPerShare.PerSecond)
		lease.joinLastRefill = now
	}
	if lease.joinTokens < 1 {
		return JoinRateExceeded
	}
	lease.joinTokens--
	return Allowed
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		Connections:          c.connections,
		Shares:               c.shares,
		PendingShares:        c.pendingShares,
		ManifestBytes:        c.manifestBytes,
		PendingManifestBytes: c.pendingManifestBytes,
		Sources:              len(c.sources),
	}
}

func (c *Controller) release(lease *Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lease.released {
		return
	}
	lease.released = true
	if lease.pins != 0 {
		return
	}
	c.releaseLeaseAccountingLocked(lease)
}

func (c *Controller) unpin(lease *Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lease.pins == 0 {
		return
	}
	lease.pins--
	if lease.pins == 0 && lease.released {
		c.releaseLeaseAccountingLocked(lease)
	}
}

func (c *Controller) releaseLeaseAccountingLocked(lease *Lease) {
	if !lease.accounted {
		return
	}
	lease.accounted = false
	switch lease.kind {
	case connectionLease:
		if c.connections > 0 {
			c.connections--
		}
	case shareLease:
		c.shares--
		c.manifestBytes -= lease.manifestBytes
		usage := c.sources[lease.source]
		usage.shares--
		usage.manifestBytes -= lease.manifestBytes
		if usage.shares == 0 && usage.pendingShares == 0 && usage.manifestBytes == 0 && usage.pendingManifestBytes == 0 {
			delete(c.sources, lease.source)
		} else {
			c.sources[lease.source] = usage
		}
	}
}
