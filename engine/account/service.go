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
	record               Record
	active               bool
	loaded               bool
	credentials          map[[16]byte]*credentialSession
	credentialDenials    map[[16]byte]string
	accountDenials       map[[16]byte]string
	state                State
	reason               string
	registrationPermille uint16
	retryAt              time.Time
	changed              chan struct{}
	wake                 chan struct{}
	start                sync.Once
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
	service := &Service{operation: make(chan struct{}, 1), config: config, ctx: ctx, cancel: cancel, state: Pending,
		changed: make(chan struct{}), wake: make(chan struct{}, 1), nodes: make(map[string]*nodeState)}
	cacheContext, cancelCache := context.WithTimeout(ctx, CacheLoadTimeout)
	record, err := service.load(cacheContext)
	cancelCache()
	if err != nil {
		service.state, service.reason = Unavailable, err.Error()
		return service, nil
	}
	service.adoptLocked(record)
	switch {
	case record.Denial != "":
		service.state, service.reason = Denied, record.Denial
	case service.validTicket(record):
		service.state = Ready
	case record.Credential == nil && record.Proof != nil:
		service.state = record.Proof.Challenge.requiredState()
	}
	return service, nil
}

func (service *Service) lockOperation(ctx context.Context) error {
	select {
	case service.operation <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (service *Service) Start() {
	if service == nil {
		return
	}
	service.start.Do(func() {
		service.mu.Lock()
		defer service.mu.Unlock()
		if service.ctx.Err() != nil {
			return
		}
		service.active = true
		service.notifyLocked()
		service.workers.Go(service.run)
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

func (service *Service) Retry() {
	if service == nil {
		return
	}
	select {
	case service.wake <- struct{}{}:
	default:
	}
	service.Start()
}

func (service *Service) notifyLocked() {
	close(service.changed)
	service.changed = make(chan struct{})
}

func (service *Service) Snapshot() (Snapshot, <-chan struct{}) {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := service.config.Now()
	value := Snapshot{Active: service.active, State: service.state, Allowance: service.record.Entitlement.Allowance(now),
		RemainingBytes: service.record.Entitlement.RemainingBytes, ResetAt: service.record.Entitlement.ResetAt(),
		ValidUntil: time.Unix(int64(service.record.Entitlement.ValidUntil), 0), RetryAt: service.retryAt,
		RegistrationPermille: service.registrationPermille, Reason: service.reason}
	// Email admission first returns an empty challenge. Only a delivered code has an ID.
	if proof := service.record.Proof; proof != nil {
		value.EmailCodePending = proof.Challenge.hasEmailCode(now)
	}
	if credential := service.record.Credential; credential != nil {
		value.Account = base64.RawURLEncoding.EncodeToString(credential.Account[:])
		value.Credential = base64.RawURLEncoding.EncodeToString(credential.ID[:])
		value.Username = credential.Username
		value.Profile = service.record.Profile
		if pending := service.record.PendingEmail; pending.Challenge.ExpiresAt > uint64(now.Unix()) && pending.Address != value.Profile.Email {
			value.PendingEmail = pending
		}
	}
	if generation, err := serviceauthority.VerifyGeneration(service.record.Certificate, service.config.Root); err == nil {
		if ticket, err := serviceauthority.VerifyTicket(service.record.Ticket, generation, now); err == nil {
			value.TicketExpires = time.Unix(int64(ticket.ExpiresAt), 0)
		}
	}
	return value, service.changed
}

func (service *Service) transition(state State, reason string, retry time.Time) {
	service.mu.Lock()
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
