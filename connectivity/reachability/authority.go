package reachability

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

type resourceKey struct {
	endpoint Endpoint
	scope    Scope
}
type resource struct {
	request        Request
	started        time.Time
	graceUntil     time.Time
	attempted      bool
	refreshPending bool
	restarts       map[string]uint32
	gateway        Gateway
	lease          Lease
	expires        time.Time
	renewAt        time.Time
	nextGateway    int
	operationUntil time.Time
	cancel         context.CancelFunc
}
type Authority struct {
	mu             sync.Mutex
	maintenance    sync.Mutex
	observation    sync.Mutex
	config         Config
	demands        map[string]Demand
	resources      map[resourceKey]*resource
	changes        chan struct{}
	retiredThrough uint64
	closed         bool
}

func New(config Config) *Authority {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Capacity <= 0 || config.Capacity > DefaultCapacity {
		config.Capacity = DefaultCapacity
	}
	if config.DemandTTL <= 0 || config.DemandTTL > DefaultDemandTTL {
		config.DemandTTL = DefaultDemandTTL
	}
	if config.LeaseTTL < time.Second || config.LeaseTTL > DefaultLeaseTTL {
		config.LeaseTTL = DefaultLeaseTTL
	}
	if config.HeadStart <= 0 {
		config.HeadStart = DefaultHeadStart
	}
	if config.Grace <= 0 {
		config.Grace = DefaultGrace
	}
	if config.OperationTimeout <= 0 {
		config.OperationTimeout = DefaultOperationTimeout
	}
	if len(config.Gateways) > DefaultCapacity {
		config.Gateways = config.Gateways[:DefaultCapacity]
	}
	config.Gateways = slices.Clone(config.Gateways)
	return &Authority{config: config, demands: make(map[string]Demand), resources: make(map[resourceKey]*resource), changes: make(chan struct{}, 1)}
}
func (a *Authority) Changes() <-chan struct{} { return a.changes }
func (a *Authority) signal() {
	select {
	case a.changes <- struct{}{}:
	default:
	}
}
func (a *Authority) emit(event Event) {
	if a.config.Observe != nil {
		a.observation.Lock()
		defer a.observation.Unlock()
		a.config.Observe(event)
	}
}
func (a *Authority) SetDemand(d Demand) error {
	now := a.config.Now()
	if d.ID == "" || !validEndpoint(d.Endpoint) || !d.Until.After(now) || !d.Content || (d.Scope.Remote.IsValid() && d.Scope.Remote.Port() == 0) {
		return ErrInvalid
	}
	if d.Until.After(now.Add(a.config.DemandTTL)) {
		d.Until = now.Add(a.config.DemandTTL)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if d.Endpoint.Generation <= a.retiredThrough {
		return ErrInvalid
	}
	if _, exists := a.demands[d.ID]; !exists && len(a.demands) >= a.config.Capacity {
		return ErrCapacity
	}
	a.demands[d.ID] = d
	a.cancelUnneeded(now)
	a.signal()
	return nil
}
func (a *Authority) Withdraw(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.demands, id)
	a.cancelUnneeded(a.config.Now())
	a.signal()
}
func (a *Authority) Retire(generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation > a.retiredThrough {
		a.retiredThrough = generation
	}
	for id, d := range a.demands {
		if d.Endpoint.Generation <= generation {
			delete(a.demands, id)
		}
	}
	a.cancelUnneeded(a.config.Now())
	a.signal()
}
func (a *Authority) needed(key resourceKey, now time.Time, existing bool) bool {
	return a.demandUntil(key, now, existing).After(now)
}
func (a *Authority) demandUntil(key resourceKey, now time.Time, existing bool) time.Time {
	var until time.Time
	for _, d := range a.demands {
		if d.Endpoint == key.endpoint && d.Scope == key.scope && d.Until.After(now) && (!d.Direct || (existing && d.RetainLease)) && d.Until.After(until) {
			until = d.Until
		}
	}
	return until
}
func (a *Authority) cancelUnneeded(now time.Time) {
	for key, r := range a.resources {
		if r.cancel != nil && a.demandUntil(key, now, r.gateway != nil).Before(r.operationUntil) {
			r.cancel()
		}
	}
}
func (a *Authority) Facts() []Fact {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.config.Now()
	facts := make([]Fact, 0, len(a.resources))
	for key, r := range a.resources {
		if key.endpoint.Generation <= a.retiredThrough || !a.needed(key, now, true) {
			continue
		}
		if key.endpoint.Local.Addr().Is6() && PublicAddress(key.endpoint.Local.Addr()) {
			facts = append(facts, Fact{Endpoint: key.endpoint, Scope: key.scope, External: key.endpoint.Local, Kind: "native-ipv6"})
		}
		if r.gateway != nil && now.Before(r.expires) {
			facts = append(facts, Fact{Endpoint: key.endpoint, Scope: key.scope, External: r.lease.External, ExpiresAt: r.expires, Kind: r.lease.Kind, GatewayID: r.lease.GatewayID})
		}
	}
	return facts
}

// Reconcile is invoked by the connectivity owner's background maintenance
// worker. Each pass services at most one gateway step per resource, concurrently:
// a slow discovery must not hold unrelated renewal or revocation deadlines.
// Capacity bounds the number of workers; serialized passes preserve single-flight.
func (a *Authority) Reconcile(ctx context.Context) {
	a.maintenance.Lock()
	defer a.maintenance.Unlock()
	a.mu.Lock()
	now := a.config.Now()
	for id, d := range a.demands {
		if !d.Until.After(now) {
			delete(a.demands, id)
		}
	}
	for _, d := range a.demands {
		key := resourceKey{d.Endpoint, d.Scope}
		if !d.Direct && a.resources[key] == nil && len(a.resources) < a.config.Capacity {
			a.resources[key] = &resource{request: Request{Endpoint: d.Endpoint, Scope: d.Scope, Lifetime: a.config.LeaseTTL}, started: now}
		}
	}
	keys := make([]resourceKey, 0, len(a.resources))
	for key := range a.resources {
		keys = append(keys, key)
	}
	a.mu.Unlock()
	var workers sync.WaitGroup
	for _, key := range keys {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.reconcileResource(ctx, key)
		}()
	}
	workers.Wait()
}
func (a *Authority) reconcileResource(ctx context.Context, key resourceKey) {
	a.mu.Lock()
	r := a.resources[key]
	now := a.config.Now()
	needed := !a.closed && a.needed(key, now, r.gateway != nil)
	if !needed {
		if r.graceUntil.IsZero() {
			r.graceUntil = now.Add(a.config.Grace)
		}
		if now.Before(r.graceUntil) && !a.closed && key.endpoint.Generation > a.retiredThrough {
			a.mu.Unlock()
			return
		}
		delete(a.resources, key)
		a.mu.Unlock()
		if r.gateway != nil {
			a.deleteLease(ctx, r)
		}
		return
	}
	r.graceUntil = time.Time{}
	if r.gateway != nil && now.Before(r.renewAt) {
		a.mu.Unlock()
		return
	}
	if ctx.Err() != nil || (r.gateway == nil && (r.attempted || now.Sub(r.started) < a.config.HeadStart)) {
		a.mu.Unlock()
		return
	}
	r.operationUntil = minTime(a.demandUntil(key, now, r.gateway != nil), now.Add(a.config.OperationTimeout))
	// Translate the injectable authority clock to a duration. This bounds the
	// real operation even when tests use a clock with a different wall epoch.
	budget := r.operationUntil.Sub(now)
	operation, cancel := context.WithTimeout(ctx, budget)
	r.cancel = cancel
	oldGateway := r.gateway
	oldLease := r.lease
	gateway := oldGateway
	if gateway == nil {
		if r.nextGateway < len(a.config.Gateways) {
			gateway = a.config.Gateways[r.nextGateway]
			r.nextGateway++
		}
		r.attempted = r.nextGateway >= len(a.config.Gateways)
	}
	a.mu.Unlock()
	var lease Lease
	err := ErrUnavailable
	issued := a.config.Now()
	if operation.Err() == nil && issued.Before(r.operationUntil) && gateway != nil {
		if oldGateway != nil {
			lease, err = gateway.Renew(operation, r.request, oldLease)
		} else {
			lease, err = gateway.Create(operation, r.request)
		}
		if err == nil && !validLease(lease, a.config.LeaseTTL) {
			a.deleteLease(operation, &resource{request: r.request, gateway: gateway, lease: lease})
			err = errors.Join(ErrInvalidResponse, ErrLeaseLost)
		}
	}
	cancel()
	if oldGateway == nil && err != nil {
		a.emit(Event{Kind: "gateway-unavailable", Endpoint: key.endpoint, Scope: key.scope, Error: err})
	}
	a.finishResource(ctx, key, r, gatewayResult{gateway: gateway, oldGateway: oldGateway, oldLease: oldLease, lease: lease, issued: issued, err: err})
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
func validLease(lease Lease, limit time.Duration) bool {
	return lease.External.IsValid() && lease.External.Port() != 0 && PublicAddress(lease.External.Addr()) && lease.Lifetime > 0 && lease.Lifetime <= limit && lease.GatewayID != "" && lease.ResourceID != ""
}
func (a *Authority) deleteLease(ctx context.Context, r *resource) {
	// Revocation has independent bounded ownership after demand or work expiry.
	operation, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.config.OperationTimeout)
	defer cancel()
	err := r.gateway.Delete(operation, r.request, r.lease)
	a.emit(Event{Kind: "lease-revoked", Endpoint: r.request.Endpoint, Scope: r.request.Scope, GatewayID: r.lease.GatewayID, Error: err})
	a.signal()
}
func (a *Authority) Close(ctx context.Context) {
	a.mu.Lock()
	a.closed = true
	clear(a.demands)
	a.cancelUnneeded(a.config.Now())
	a.mu.Unlock()
	a.Reconcile(ctx)
}

type gatewayResult struct {
	gateway    Gateway
	oldGateway Gateway
	oldLease   Lease
	lease      Lease
	issued     time.Time
	err        error
}

func (a *Authority) finishResource(ctx context.Context, key resourceKey, r *resource, result gatewayResult) {
	gateway, oldGateway, oldLease := result.gateway, result.oldGateway, result.oldLease
	lease, issued, err := result.lease, result.issued, result.err
	a.mu.Lock()
	r.cancel = nil
	now := a.config.Now()
	// A reply already in flight at a sibling's restart must not resurrect the
	// old server epoch after that sibling invalidated published authority.
	if epoch, restarted := r.restarts[lease.GatewayID]; err == nil && restarted && lease.ServerEpoch > epoch && !lease.ServerRestarted {
		r.restarts = nil
		r.renewAt = now
		if oldGateway == nil {
			r.nextGateway = max(0, r.nextGateway-1)
			r.attempted = false
		}
		a.mu.Unlock()
		a.signal()
		a.emit(Event{Kind: "lease-superseded", Endpoint: key.endpoint, Scope: key.scope, GatewayID: lease.GatewayID})
		return
	}
	r.restarts = nil
	stillNeeded := !a.closed && a.needed(key, now, oldGateway != nil) && key.endpoint.Generation > a.retiredThrough
	if err != nil || gateway == nil || !validLease(lease, a.config.LeaseTTL) {
		lost := a.failResource(r, oldGateway, stillNeeded, err, now)
		a.mu.Unlock()
		if lost {
			a.signal()
			a.emit(Event{Kind: "lease-lost", Endpoint: key.endpoint, Scope: key.scope, GatewayID: oldLease.GatewayID, Error: err})
		}
		a.emit(Event{Kind: "lease-failed", Endpoint: key.endpoint, Scope: key.scope, Error: err})
		return
	}
	if lease.ServerRestarted {
		a.invalidateRestartedLeases(key, r, lease, now)
	}
	r.refreshPending = false
	r.gateway = gateway
	r.lease = lease
	// Lease time starts at the request, conservatively accounting for response
	// latency instead of extending gateway authority by the round-trip duration.
	r.expires = issued.Add(lease.Lifetime)
	r.renewAt = issued.Add(lease.Lifetime / 2)
	stillNeeded = stillNeeded && now.Before(r.expires)
	if !stillNeeded {
		delete(a.resources, key)
	}
	a.mu.Unlock()
	if !stillNeeded {
		a.deleteLease(ctx, r)
		return
	}
	a.emit(Event{Kind: "lease-ready", Endpoint: key.endpoint, Scope: key.scope, GatewayID: lease.GatewayID, ServerEpoch: lease.ServerEpoch, ServerRestarted: lease.ServerRestarted})
	a.signal()
}

// Caller holds a.mu while changing the retry or published lease authority.
func (a *Authority) failResource(r *resource, oldGateway Gateway, stillNeeded bool, err error, now time.Time) bool {
	if r.refreshPending {
		r.attempted = false
		r.nextGateway = 0
		r.refreshPending = false
	}
	if oldGateway == nil && stillNeeded && errors.Is(err, context.Canceled) {
		// A shorter equivalent demand may cancel an in-flight probe while
		// another live owner still authorizes a new, shorter operation.
		r.nextGateway = max(0, r.nextGateway-1)
		r.attempted = false
	}
	lost := oldGateway != nil && errors.Is(err, ErrLeaseLost)
	if lost {
		r.gateway = nil
		r.lease = Lease{}
		r.expires = time.Time{}
		r.nextGateway = 0
		r.attempted = false
	} else if oldGateway != nil {
		r.renewAt = now.Add(a.config.OperationTimeout)
	}
	return lost
}

// Caller holds a.mu so sibling replies cannot publish an obsolete server epoch.
func (a *Authority) invalidateRestartedLeases(key resourceKey, r *resource, lease Lease, now time.Time) {
	for otherKey, other := range a.resources {
		if other == r || otherKey.endpoint.Egress != key.endpoint.Egress {
			continue
		}
		if other.cancel != nil {
			if other.restarts == nil {
				other.restarts = make(map[string]uint32)
			}
			other.restarts[lease.GatewayID] = lease.ServerEpoch
		}
		if other.lease.GatewayID == lease.GatewayID && other.lease.ServerEpoch > lease.ServerEpoch {
			other.expires = now
			other.renewAt = now
		}
	}
}
