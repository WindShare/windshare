package turnpath

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/turncontrol"
)

const (
	MaximumActiveServices   = 2
	MaximumReplacementPaths = 1
	ServiceCooldown         = 30 * time.Second
	ReplacementBudget       = 90 * time.Second
	MaintenanceInterval     = time.Second
	MaximumRetiredPaths     = 128
	AdvertisementInterval   = 5 * time.Second
	ControlWriteBudget      = 5 * time.Second
)

type SenderConfig struct {
	Accounts  Accounts
	SessionID [16]byte
	Send      func(context.Context, []byte) error
	Retire    func([16]byte)
	Now       func() time.Time
	Random    io.Reader
	Tick      <-chan time.Time
	Observe   func(Event)
}
type Event struct {
	At          time.Time
	Subject     Subject
	Provider    string
	Account     string
	Profile     string
	Milestone   string
	Reason      string
	UsefulBytes uint64
}
type service struct {
	profile          Profile
	current, pending *delegation
	cancel           context.CancelFunc
	retryAt          time.Time
}
type delegation struct {
	lease    *Lease
	grant    turncontrol.Grant
	created  time.Time
	admitted bool
	ready    bool
	useful   uint64
}
type issued struct {
	service    *service
	path       [16]byte
	sequence   uint64
	generation uint64
	lease      *Lease
	err        error
}
type Sender struct {
	config       SenderConfig
	mu           sync.Mutex
	paths        map[[16]byte]*delegation
	retired      map[[16]byte]time.Time
	controls     chan turncontrol.Message
	completed    chan issued
	work         sync.WaitGroup
	services     []*service
	owner        [16]byte
	sequence     uint64
	generation   uint64
	demandUntil  time.Time
	advertisedAt time.Time
}

func NewSender(config SenderConfig) *Sender {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	s := &Sender{config: config, paths: make(map[[16]byte]*delegation), retired: make(map[[16]byte]time.Time),
		controls: make(chan turncontrol.Message, 32), completed: make(chan issued, 16)}
	if config.Accounts != nil {
		for _, profile := range config.Accounts.Profiles() {
			s.services = append(s.services, &service{profile: profile})
		}
	}
	if len(s.services) > 0 {
		if _, err := io.ReadFull(config.Random, s.owner[:]); err != nil {
			s.services = nil
		}
	}
	return s
}

func (s *Sender) Handle(body []byte) error {
	message, err := turncontrol.Decode(body)
	if err != nil || message.Header().SessionID != s.config.SessionID {
		return turncontrol.ErrMessage
	}
	switch value := message.(type) {
	case turncontrol.Demand, turncontrol.Ready, turncontrol.Report:
	case turncontrol.Retire:
		if value.Reason != turncontrol.Unhelpful && value.Reason != turncontrol.Idle && value.Reason != turncontrol.Expired {
			return turncontrol.ErrMessage
		}
	default:
		return turncontrol.ErrMessage
	}
	select {
	case s.controls <- message:
	default:
	}
	return nil
}

// AuthorizedPath binds delegated credentials to their authenticated predecessor.
type AuthorizedPath struct {
	*Lease
	ReplacesPeerPathID [16]byte
}

// Lookup is the sender's authority check before constructing a native attempt.
// A retired grant cannot fall through to ordinary direct-path preparation.
func (s *Sender) Lookup(path [16]byte) (*AuthorizedPath, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, retired := s.retired[path]; retired {
		return nil, true, errors.New("TURN path retired")
	}
	value := s.paths[path]
	if value == nil {
		return nil, false, nil
	}
	now := s.config.Now()
	if !value.lease.Relay().Credentials().ValidAt(now) ||
		(value.grant.CredentialKind == turncontrol.Expiring && !now.Before(value.created.Add(value.grant.ValidFor))) {
		return nil, true, errors.New("TURN path expired")
	}
	select {
	case <-value.lease.Lost():
		return nil, true, errors.New("TURN account unavailable")
	default:
	}
	return &AuthorizedPath{Lease: value.lease, ReplacesPeerPathID: value.grant.Replaces}, true, nil
}
func (s *Sender) Admitted(path [16]byte) {
	s.mu.Lock()
	if value := s.paths[path]; value != nil {
		value.admitted = true
	}
	s.mu.Unlock()
}

func (s *Sender) Run(ctx context.Context) {
	var ticker *time.Ticker
	tick := s.config.Tick
	if tick == nil {
		ticker = time.NewTicker(MaintenanceInterval)
		tick = ticker.C
		defer ticker.Stop()
	}
	defer s.close()
	s.advertise(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-s.controls:
			s.apply(ctx, message)
		case result := <-s.completed:
			s.install(ctx, result)
		case <-tick:
			s.maintain(ctx)
		}
	}
}

func (s *Sender) apply(ctx context.Context, message turncontrol.Message) {
	header := message.Header()
	if s.owner != ([16]byte{}) && header.OwnerPathID != s.owner {
		return
	}
	switch value := message.(type) {
	case turncontrol.Demand:
		if header.Sequence <= s.sequence {
			return
		}
		s.owner = header.OwnerPathID
		s.sequence = header.Sequence
		if value.HoldFor == 0 || !s.config.Now().Before(s.demandUntil) {
			s.generation++
		}
		s.demandUntil = s.config.Now().Add(value.HoldFor)
		s.maintain(ctx)
	case turncontrol.Ready:
		for _, service := range s.services {
			pending := service.pending
			if pending == nil || pending.grant.PathID != value.PathID || pending.grant.Authority != header {
				continue
			}
			pending.ready = true
			s.promote(ctx, service)
		}
	case turncontrol.Report:
		s.mu.Lock()
		path := s.paths[value.PathID]
		s.mu.Unlock()
		if path != nil && path.grant.Authority == header && value.UsefulBytes > path.useful {
			path.lease.Charge(Usage{Useful: value.UsefulBytes - path.useful})
			path.useful = value.UsefulBytes
			s.observe(path, "useful_content", "")
		}
	case turncontrol.Retire:
		retryAt := s.config.Now()
		if value.Reason != turncontrol.Idle {
			retryAt = retryAt.Add(ServiceCooldown)
		}
		for _, service := range s.services {
			if current := service.current; current != nil && current.grant.PathID == value.PathID && current.grant.Authority == header {
				s.retire(ctx, current, value.Reason)
				service.current = nil
				service.retryAt = retryAt
			}
			if pending := service.pending; pending != nil && pending.grant.PathID == value.PathID && pending.grant.Authority == header {
				s.retire(ctx, pending, value.Reason)
				service.pending = nil
				service.retryAt = retryAt
			}
		}
	}
}

func (s *Sender) maintain(ctx context.Context) {
	now := s.config.Now()
	if s.sequence == 0 && now.Sub(s.advertisedAt) >= AdvertisementInterval {
		s.advertise(ctx)
	}
	demand := now.Before(s.demandUntil)
	for _, service := range s.services {
		s.promote(ctx, service)
		s.retireUnusable(ctx, service, now, demand)
		if !demand && service.cancel != nil {
			service.cancel()
		}
	}
	if !demand {
		return
	}
	active, overlap := 0, 0
	for _, service := range s.services {
		if service.current != nil || service.pending != nil || service.cancel != nil {
			active++
		}
		if service.current != nil && (service.pending != nil || service.cancel != nil) {
			overlap++
		}
	}
	for _, service := range s.services {
		if service.pending != nil || service.cancel != nil || now.Before(service.retryAt) {
			continue
		}
		if service.current == nil {
			if active >= MaximumActiveServices {
				continue
			}
			active++
		} else {
			if !service.current.renewalDue(now) || overlap >= MaximumReplacementPaths {
				continue
			}
			overlap++
		}
		s.acquire(ctx, service)
	}
}

func (value *delegation) renewalDue(now time.Time) bool {
	credential := value.lease.Relay().Credentials()
	if !value.lease.Renewable() || credential.ExpiresAt().IsZero() {
		return false
	}
	lifetime := credential.ExpiresAt().Sub(value.created)
	renewAt := value.created.Add(max(lifetime/2, lifetime-ReplacementBudget*2))
	return !now.Before(renewAt)
}

func (value *delegation) retirementReason(now time.Time, demand, pending bool) turncontrol.RetireReason {
	switch {
	case !demand:
		return turncontrol.Idle
	case !value.lease.Relay().Credentials().ValidAt(now),
		value.grant.CredentialKind == turncontrol.Expiring && !now.Before(value.created.Add(value.grant.ValidFor)):
		return turncontrol.Expired
	case pending && now.Sub(value.created) >= ReplacementBudget:
		return turncontrol.Unhelpful
	default:
		select {
		case <-value.lease.Lost():
			return turncontrol.BudgetExhausted
		default:
			return 0
		}
	}
}

func (s *Sender) retireUnusable(ctx context.Context, service *service, now time.Time, demand bool) {
	for _, value := range []*delegation{service.current, service.pending} {
		if value == nil {
			continue
		}
		reason := value.retirementReason(now, demand, value == service.pending)
		if reason == 0 {
			continue
		}
		s.retire(ctx, value, reason)
		if service.current == value {
			service.current = nil
		} else {
			service.pending = nil
		}
		if reason != turncontrol.Idle {
			service.retryAt = now.Add(ServiceCooldown)
		}
	}
}

func (s *Sender) acquire(ctx context.Context, service *service) {
	var path [16]byte
	if _, err := io.ReadFull(s.config.Random, path[:]); err != nil || path == ([16]byte{}) {
		service.retryAt = s.config.Now().Add(ServiceCooldown)
		return
	}
	child, cancel := context.WithCancel(ctx)
	service.cancel = cancel
	sequence := s.sequence
	generation := s.generation
	current := service.current
	s.observe(&delegation{grant: turncontrol.Grant{PathID: path, Provider: service.profile.Identity.Provider,
		Account: service.profile.Identity.Account, Profile: service.profile.Identity.Profile}}, "issuance_started", "")
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		subject := Subject{SessionID: s.config.SessionID, PathID: path}
		var lease *Lease
		var err error
		if current != nil {
			lease, err = current.lease.Renew(child, subject)
		} else {
			lease, err = s.config.Accounts.Acquire(child, service.profile.Identity.Profile, subject)
		}
		result := issued{service: service, path: path, sequence: sequence, generation: generation, lease: lease, err: err}
		select {
		case s.completed <- result:
		case <-ctx.Done():
			if lease != nil {
				lease.Close()
			}
		}
	}()
}

func (s *Sender) install(ctx context.Context, result issued) {
	service := result.service
	service.cancel()
	service.cancel = nil
	if result.err != nil || result.generation != s.generation || !s.config.Now().Before(s.demandUntil) {
		if result.lease != nil {
			result.lease.Close()
		}
		service.retryAt = s.config.Now().Add(ServiceCooldown)
		if result.err != nil {
			s.observe(&delegation{grant: turncontrol.Grant{PathID: result.path, Provider: service.profile.Identity.Provider,
				Account: service.profile.Identity.Account, Profile: service.profile.Identity.Profile}}, "issuance_failed", result.err.Error())
			s.send(ctx, turncontrol.Unavailable{Authority: s.header(), Profile: service.profile.Identity.Profile,
				RetryAfter: ServiceCooldown, Reason: result.err.Error()})
		}
		return
	}
	relay := result.lease.Relay()
	credential := relay.Credentials()
	grant := turncontrol.Grant{Authority: turncontrol.Header{SessionID: s.config.SessionID, OwnerPathID: s.owner, Sequence: result.sequence},
		PathID: result.path, Provider: relay.Identity().Provider, Account: relay.Identity().Account, Profile: relay.Identity().Profile,
		Username: credential.Username(), Password: credential.Password(), URLs: relay.URLs(), CredentialKind: turncontrol.Fixed}
	if !credential.ExpiresAt().IsZero() {
		grant.CredentialKind = turncontrol.Expiring
		grant.ValidFor = min(credential.ExpiresAt().Sub(s.config.Now()).Truncate(time.Millisecond), turncontrol.MaximumCredentialLifetime)
	}
	if service.current != nil {
		grant.Replaces = service.current.grant.PathID
	}
	value := &delegation{lease: result.lease, grant: grant, created: s.config.Now()}
	s.mu.Lock()
	s.paths[grant.PathID] = value
	s.mu.Unlock()
	service.pending = value
	if !s.send(ctx, grant) {
		s.retire(ctx, value, turncontrol.Unhelpful)
		service.pending = nil
		service.retryAt = s.config.Now().Add(ServiceCooldown)
	}
	s.observe(value, "credentials_delegated", "")
}

func (s *Sender) promote(ctx context.Context, service *service) {
	pending := service.pending
	if pending == nil || !pending.ready {
		return
	}
	now := s.config.Now()
	if !now.Before(s.demandUntil) || !pending.lease.Relay().Credentials().ValidAt(now) ||
		now.Sub(pending.created) >= ReplacementBudget {
		return
	}
	select {
	case <-pending.lease.Lost():
		return
	default:
	}
	s.mu.Lock()
	admitted := pending.admitted
	s.mu.Unlock()
	if !admitted {
		return
	}
	old := service.current
	service.current, service.pending = pending, nil
	if old != nil {
		s.retire(ctx, old, turncontrol.Replaced)
	}
	s.observe(pending, "replacement_admitted", "")
}

func (s *Sender) retire(ctx context.Context, value *delegation, reason turncontrol.RetireReason) {
	// Withdraw lookup authority before closing sockets. Late offers cannot race
	// teardown into allocating another connection with the same credential.
	s.mu.Lock()
	delete(s.paths, value.grant.PathID)
	now := s.config.Now()
	for path, until := range s.retired {
		if !now.Before(until) {
			delete(s.retired, path)
		}
	}
	if len(s.retired) < MaximumRetiredPaths {
		s.retired[value.grant.PathID] = now.Add(turncontrol.MaximumDemandLifetime)
	}
	s.mu.Unlock()
	s.send(ctx, turncontrol.Retire{Authority: value.grant.Authority, PathID: value.grant.PathID, Reason: reason})
	if s.config.Retire != nil {
		s.config.Retire(value.grant.PathID)
	}
	value.lease.Close()
	s.observe(value, "path_retired", fmt.Sprint(reason))
}
func (s *Sender) close() {
	for _, service := range s.services {
		if service.cancel != nil {
			service.cancel()
		}
	}
	s.work.Wait()
	for {
		select {
		case result := <-s.completed:
			if result.lease != nil {
				result.lease.Close()
			}
		default:
			for _, service := range s.services {
				for _, value := range []*delegation{service.current, service.pending} {
					if value != nil {
						if s.config.Retire != nil {
							s.config.Retire(value.grant.PathID)
						}
						value.lease.Close()
					}
				}
			}
			return
		}
	}
}
func (s *Sender) header() turncontrol.Header {
	return turncontrol.Header{SessionID: s.config.SessionID, OwnerPathID: s.owner, Sequence: s.sequence}
}
func (s *Sender) send(ctx context.Context, message turncontrol.Message) bool {
	if s.config.Send == nil {
		return false
	}
	body, err := turncontrol.Encode(message)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, ControlWriteBudget)
	defer cancel()
	return s.config.Send(ctx, body) == nil
}

func (s *Sender) advertise(ctx context.Context) {
	if len(s.services) == 0 {
		return
	}
	profiles := make([]string, 0, len(s.services))
	for _, service := range s.services {
		profiles = append(profiles, service.profile.Identity.Profile)
	}
	s.send(ctx, turncontrol.Available{Authority: turncontrol.Header{
		SessionID: s.config.SessionID, OwnerPathID: s.owner, Sequence: 1,
	}, Profiles: profiles})
	s.advertisedAt = s.config.Now()
}
func (s *Sender) observe(value *delegation, milestone, reason string) {
	slog.Debug("TURN path", "provider", value.grant.Provider, "account", value.grant.Account, "profile", value.grant.Profile,
		"session_id", fmt.Sprintf("%x", s.config.SessionID), "path_id", fmt.Sprintf("%x", value.grant.PathID),
		"milestone", milestone, "reason", reason, "useful_bytes", value.useful)
	if s.config.Observe != nil {
		s.config.Observe(Event{At: s.config.Now(), Subject: Subject{SessionID: s.config.SessionID, PathID: value.grant.PathID},
			Provider: value.grant.Provider, Account: value.grant.Account, Profile: value.grant.Profile,
			Milestone: milestone, Reason: reason, UsefulBytes: value.useful})
	}
}
