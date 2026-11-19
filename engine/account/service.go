package account

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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
	nodes                map[string]*nodeState
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
	service := &Service{operation: make(chan struct{}, 1), local: make(chan struct{}, 1),
		config: config, ctx: ctx, cancel: cancel,
		changed: make(chan struct{}), wake: make(chan struct{}, 1), nodes: make(map[string]*nodeState)}
	defer service.workers.Go(service.run)
	cacheContext, cancelCache := context.WithTimeout(ctx, CacheLoadTimeout)
	record, err := service.load(cacheContext)
	cancelCache()
	if err != nil {
		service.state, service.reason = Unavailable, err.Error()
		return service, nil
	}
	service.adoptLocked(record)
	return service, nil
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
	value := Snapshot{Active: service.refreshScopeLocked() != refreshInactive, State: service.state, Allowance: service.record.Entitlement.Allowance(now),
		RemainingBytes: service.record.Entitlement.RemainingBytes, ResetAt: service.record.Entitlement.ResetAt(),
		ValidUntil: time.Unix(int64(service.record.Entitlement.ValidUntil), 0), RetryAt: service.retryAt,
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
		value.TicketExpires = time.Unix(int64(ticket.ExpiresAt), 0)
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
	if !sameIdentity(service.record, record) {
		service.mu.Unlock()
		slog.Debug("account_state_transition_superseded", "state", state)
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
		slog.Info("account_state_changed", "from", previous, "to", state, "reason", reason, "retry_at", retry)
	}
}
