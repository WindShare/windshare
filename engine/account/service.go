package account

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

var ErrNotConfigured = errors.New("official account service is not configured")
var ErrSuperseded = errors.New("account changed while the operation was in progress")

type Service struct {
	config               Config
	ctx                  context.Context
	cancel               context.CancelFunc
	mu                   sync.Mutex
	operation            chan struct{}
	local                chan struct{}
	record               Record
	officialShares       map[*ShareAuthorization]struct{}
	accountViews         int
	unconfirmedShares    map[*ShareAuthorization]struct{}
	credentials          map[[16]byte]*credentialSession
	credentialDenials    map[[16]byte]string
	accountDenials       map[[16]byte]string
	state                State
	reason               string
	registrationPermille uint16
	retryAt              time.Time
	changed              chan struct{}
	wake                 chan struct{}
	workers              sync.WaitGroup
	nodes                *nodePolicyCache
	verifiedCertificate  string
}

func New(config Config) (*Service, error) {
	address, err := url.Parse(config.URL)
	if err != nil || address.Hostname() == "" || address.User != nil || address.RawQuery != "" ||
		address.Fragment != "" || strings.HasSuffix(address.Hostname(), ".workers.dev") ||
		address.Scheme != "https" && (address.Scheme != "http" || (address.Hostname() != "localhost" && !net.ParseIP(address.Hostname()).IsLoopback())) ||
		config.Root == [32]byte{} {
		return nil, errors.New("invalid official account authority")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Store == nil {
		config.Store = &MemoryStore{}
	}
	if config.Client == nil {
		config.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		operation: make(chan struct{}, 1),
		local:     make(chan struct{}, 1),
		config:    config,
		ctx:       ctx,
		cancel:    cancel,
		changed:   make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}
	service.nodes = newNodePolicyCache(
		ctx,
		&service.workers,
		config.Client,
		config.Now,
		service.resolveOfficialNode,
		service.notify,
		config.Diagnostics,
	)
	cacheContext, cancelCache := context.WithTimeout(ctx, CacheLoadTimeout)
	record, err := service.load(cacheContext)
	cancelCache()
	service.mu.Lock()
	if err != nil {
		if isLocalStoreError(err) {
			service.state, service.reason = StorageError, err.Error()
		} else {
			service.state, service.reason = Unavailable, err.Error()
		}
	} else {
		service.adoptLocked(record)
	}
	service.mu.Unlock()
	service.workers.Go(service.run)
	return service, nil
}

func (service *Service) resolveOfficialNode(address string) (Node, bool) {
	if service.config.ResolveNode == nil {
		return Node{}, false
	}
	return service.config.ResolveNode(address)
}

func (service *Service) notify() {
	service.mu.Lock()
	service.notifyLocked()
	service.mu.Unlock()
}

func (service *Service) lockOperation(ctx context.Context) error {
	return acquire(ctx, service.operation)
}

// The local gate orders store reads and writes with their in-memory projections
// without making share creation wait for a remote account operation.
func (service *Service) lockLocal(ctx context.Context) error {
	return acquire(ctx, service.local)
}

func acquire(ctx context.Context, gate chan<- struct{}) error {
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ObserveAccount maintains the selected account for the lifetime of a visible view.
func (service *Service) ObserveAccount() func() {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.ctx.Err() != nil {
		return func() {}
	}
	service.accountViews++
	service.refreshDemandChangedLocked()
	return sync.OnceFunc(func() {
		service.mu.Lock()
		defer service.mu.Unlock()
		service.accountViews--
		service.refreshDemandChangedLocked()
	})
}

func (service *Service) Close() {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.cancel()
	service.mu.Unlock()
	service.workers.Wait()
}

func (service *Service) notifyLocked() {
	close(service.changed)
	service.changed = make(chan struct{})
}

func (service *Service) Snapshot() (Snapshot, <-chan struct{}) {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := service.config.Now()
	var validUntil time.Time
	if service.record.Entitlement.Version > 0 && service.record.Entitlement.ValidUntil > 0 {
		validUntil = time.Unix(int64(service.record.Entitlement.ValidUntil), 0).UTC()
	}
	allowance := service.record.Entitlement.Allowance(now)
	var remainingBytes uint64
	var resetAt time.Time
	// Stale entitlements may belong to an elapsed UTC day whose reset time is already in the past.
	if allowance != serviceauthority.AllowanceStale {
		remainingBytes = service.record.Entitlement.RemainingBytes
		resetAt = service.record.Entitlement.ResetAt()
	}
	value := Snapshot{Active: service.refreshDemandLocked().active(), State: service.state, Allowance: allowance,
		RemainingBytes: remainingBytes, ResetAt: resetAt,
		ValidUntil: validUntil, RetryAt: service.retryAt,
		RegistrationPermille: service.registrationPermille, Reason: service.reason}
	// Email admission first returns an empty challenge. Only a delivered code has an ID.
	if proof := service.record.Proof; proof != nil {
		value.EmailCodePending = proof.Challenge.hasEmailCode(now)
	}
	credential := service.record.Credential
	if credential != nil {
		value.Account = base64.RawURLEncoding.EncodeToString(credential.Account[:])
		value.Credential = base64.RawURLEncoding.EncodeToString(credential.ID[:])
		value.Username = credential.Username
		value.Profile = service.record.Profile
		if pending := service.record.PendingEmail; pending.Challenge.ExpiresAt > uint64(now.Unix()) && pending.Address != value.Profile.Email {
			value.PendingEmail = pending
		}
	}
	ticket, err := service.verifyTicket(service.record, now)
	if err == nil {
		value.TicketExpires = time.Unix(int64(ticket.ExpiresAt), 0).UTC()
	}
	// Cached authorization can age without a worker transition.
	switch value.State {
	case Pending, Ready, RefreshRequired:
		switch {
		case credential == nil:
			value.State = Pending
		case service.record.ownsTicket(ticket) && value.TicketExpires.After(now.Add(TicketMargin)):
			value.State = Ready
		default:
			value.State = RefreshRequired
		}
	}
	return value, service.changed
}

func (service *Service) transition(record Record, state State, reason string, retry time.Time) {
	service.mu.Lock()
	// A local read can adopt a new selection while an authority request is in flight.
	if !sameIdentity(service.record, record) || service.state == StorageError {
		service.mu.Unlock()
		if service.config.Diagnostics.Enabled() {
			service.emitDiagnostic("account_state_transition_superseded", diagnostics.Fields{
				"state": diagnostics.Text(string(state)),
			})
		}
		return
	}
	previous := service.state
	if service.record.Denial != "" {
		state, reason = Denied, service.record.Denial
	}
	service.state, service.reason, service.retryAt = state, reason, retry
	if state != Registering {
		service.registrationPermille = 0
	}
	service.notifyLocked()
	service.mu.Unlock()
	if previous != state {
		service.emitStateChanged(previous, state, reason, retry, record.Credential)
	}
}

func (service *Service) emitDiagnostic(name string, fields diagnostics.Fields) {
	service.config.Diagnostics.Emit(diagnostics.New(name, fields).At(service.config.Now()))
}

func (service *Service) emitStateChanged(from, to State, reason string, retry time.Time, credential *Credential) {
	if !service.config.Diagnostics.Enabled() {
		return
	}
	fields := diagnostics.Fields{
		"from": diagnostics.Text(string(from)),
		"to":   diagnostics.Text(string(to)),
	}
	if reason != "" {
		fields["reason"] = diagnostics.Text(reason)
	}
	timestampField(fields, "retry_at", retry)
	credentialFields(fields, credential)
	service.emitDiagnostic("account_state_changed", fields)
}

func (service *Service) emitTicketIssued(record Record, expiresAt uint64, login bool) {
	if !service.config.Diagnostics.Enabled() {
		return
	}
	fields := diagnostics.Fields{
		"login":               diagnostics.Bool(login),
		"entitlement_version": diagnostics.Uint(record.Entitlement.Version),
		"entitlement_day":     diagnostics.Uint(record.Entitlement.Day),
		"remaining_bytes":     diagnostics.Uint(record.Entitlement.RemainingBytes),
	}
	credentialFields(fields, record.Credential)
	if expiresAt > 0 {
		timestampField(fields, "expires_at", time.Unix(int64(expiresAt), 0))
	}
	if record.Entitlement.ValidUntil > 0 {
		timestampField(fields, "valid_until", time.Unix(int64(record.Entitlement.ValidUntil), 0))
	}
	service.emitDiagnostic("account_ticket_issued", fields)
}

func timestampField(fields diagnostics.Fields, key string, at time.Time) {
	if !at.IsZero() {
		fields[key] = diagnostics.Text(at.UTC().Format(time.RFC3339Nano))
	}
}

func credentialFields(fields diagnostics.Fields, credential *Credential) {
	if credential == nil {
		return
	}
	fields["account"] = diagnostics.Identity(credential.Account[:])
	fields["credential"] = diagnostics.Identity(credential.ID[:])
}
