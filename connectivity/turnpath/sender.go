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
	Accounts          Accounts
	SessionID         [16]byte
	Send              func(context.Context, []byte) error
	Retire            func([16]byte)
	Now               func() time.Time
	Random            io.Reader
	Tick              <-chan time.Time
	Observe           func(Event)
	NetworkGeneration func() uint64
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
	recoveryAttempts int
}
type delegation struct {
	lease          *Lease
	grant          turncontrol.Grant
	created        time.Time
	admission      *Admission
	admissionEpoch uint32
	readyLane      turncontrol.Lane
	reported       uint64
	useful         uint64
	recovery       turncontrol.RecoveryReason
	recoveryUntil  time.Time
}
type issued struct {
	service             *service
	path                [16]byte
	sequence            uint64
	generation          uint64
	profileRevision     uint64
	requestCancellation error
	lease               *Lease
	err                 error
}
type Sender struct {
	config             SenderConfig
	mu                 sync.Mutex
	paths              map[[16]byte]*delegation
	retired            map[[16]byte]time.Time
	controls           *controlMailbox
	completed          chan issued
	work               sync.WaitGroup
	services           []*service
	owner              [16]byte
	sequence           uint64
	generation         uint64
	demandUntil        time.Time
	advertisedAt       time.Time
	advertisedRevision uint64
	inventory          Inventory
	reconcileWake      chan struct{}
	networkGeneration  uint64
}

func NewSender(config SenderConfig) *Sender {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	s := &Sender{config: config, paths: make(map[[16]byte]*delegation), retired: make(map[[16]byte]time.Time),
		controls: newControlMailbox(), completed: make(chan issued, 16), reconcileWake: make(chan struct{}, 1)}
	if config.Accounts != nil {
		s.inventory = config.Accounts.Inventory()
		for _, profile := range s.inventory.Profiles {
			s.services = append(s.services, &service{profile: profile})
		}
		if _, err := io.ReadFull(config.Random, s.owner[:]); err != nil {
			s.services = nil
			s.config.Accounts = nil
			s.inventory = Inventory{}
		}
	}
	return s
}

func (s *Sender) Handle(ctx context.Context, body []byte) error {
	message, err := turncontrol.Decode(body)
	if err != nil || message.Header().SessionID != s.config.SessionID {
		return turncontrol.ErrMessage
	}
	if err := s.controls.check(ctx); err != nil {
		return err
	}
	switch value := message.(type) {
	case turncontrol.Report:
		s.retainReport(value)
		return nil
	case turncontrol.Recover:
		s.applyRecovery(value)
		return nil
	case turncontrol.Demand, turncontrol.Ready:
	case turncontrol.Retire:
		if value.Reason != turncontrol.Unhelpful && value.Reason != turncontrol.Idle && value.Reason != turncontrol.Expired {
			return turncontrol.ErrMessage
		}
	default:
		return turncontrol.ErrMessage
	}
	return s.controls.put(ctx, message)
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
	if err := s.controls.check(context.Background()); err != nil {
		return nil, true, err
	}
	if _, retired := s.retired[path]; retired {
		return nil, true, errors.New("TURN path retired")
	}
	value := s.paths[path]
	if value == nil {
		return nil, false, nil
	}
	if value.recovery != 0 {
		// Recovery owns the next allocation. Reusing this grant could admit a
		// new connection just before the damaged predecessor is retired.
		return nil, true, errors.New("TURN path recovery pending")
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
		case <-s.controls.wake:
			if message, ok := s.controls.take(); ok {
				s.apply(ctx, message)
			}
		case result := <-s.completed:
			s.install(ctx, result)
		case <-s.inventory.Changed:
			s.maintain(ctx)
		case <-tick:
			s.maintain(ctx)
		case <-s.reconcileWake:
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
		s.applyDemand(ctx, value)
	case turncontrol.Ready:
		for _, service := range s.services {
			pending := service.pending
			if pending == nil || pending.grant.PathID != value.PathID || pending.grant.Authority != header {
				continue
			}
			if value.Lane.Epoch >= pending.readyLane.Epoch {
				pending.readyLane = value.Lane
			}
			s.promote(ctx, service)
		}
	case turncontrol.Recover:
		s.applyRecovery(value)
	case turncontrol.Report:
		s.retainReport(value)
		s.applyReports()
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
				s.countInitialRecoveryFailure(service, pending)
				s.retire(ctx, pending, value.Reason)
				service.pending = nil
				service.retryAt = retryAt
			}
		}
	}
}

func (s *Sender) maintain(ctx context.Context) {
	s.applyReports()
	s.refreshConfiguration(ctx)
	s.refreshRecoveryNetwork()
	now := s.config.Now()
	if s.advertisedRevision != s.inventory.Revision || s.sequence == 0 && now.Sub(s.advertisedAt) >= AdvertisementInterval {
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
		if !s.canAcquire(service, now, active, overlap) {
			continue
		}
		if service.current == nil {
			active++
		} else {
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
			if errors.Is(value.lease.Loss(), ErrBudget) {
				return turncontrol.BudgetExhausted
			}
			return turncontrol.ProviderRevoked
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
		s.mu.Lock()
		if !value.recoveryUntil.IsZero() && !now.Before(value.recoveryUntil) {
			reason = turncontrol.Unhelpful
		}
		s.mu.Unlock()
		if reason == 0 {
			continue
		}
		s.countInitialRecoveryFailure(service, value)
		s.retire(ctx, value, reason)
		if service.current == value {
			service.current = nil
		} else {
			service.pending = nil
		}
		if reason == turncontrol.Unhelpful || reason == turncontrol.Expired {
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
	profile := service.profile
	current := service.current
	recovery := s.needsRecovery(current) && !current.renewalDue(s.config.Now())
	if recovery {
		service.recoveryAttempts++
	}
	s.observe(&delegation{grant: turncontrol.Grant{PathID: path, Provider: service.profile.Identity.Provider,
		Account: service.profile.Identity.Account, Profile: service.profile.Identity.Profile}}, "issuance_started", "")
	s.work.Go(func() {
		subject := Subject{SessionID: s.config.SessionID, PathID: path}
		var lease *Lease
		var err error
		if current != nil && !recovery {
			lease, err = current.lease.Renew(child, subject)
		} else {
			lease, err = s.config.Accounts.Acquire(child, profile.Identity.Profile, subject)
		}
		result := issued{service: service, path: path, sequence: sequence, generation: generation, profileRevision: profile.Revision, requestCancellation: context.Cause(child), lease: lease, err: err}
		select {
		case s.completed <- result:
		case <-ctx.Done():
			if lease != nil {
				lease.Close()
			}
		}
	})
}

func (s *Sender) install(ctx context.Context, result issued) {
	s.refreshConfiguration(ctx)
	service := result.service
	service.cancel()
	service.cancel = nil
	if reason := s.discardReason(result); reason != "" {
		if result.lease != nil {
			result.lease.Close()
		}
		s.observeIssuance(result, "issuance_discarded", reason)
		// Completion releases the single in-flight slot. Reconcile fresh demand
		// now rather than imposing either a failure cooldown or a maintenance tick.
		s.maintain(ctx)
		return
	}
	if result.err != nil {
		if result.lease != nil {
			result.lease.Close()
		}
		service.retryAt = s.config.Now().Add(ServiceCooldown)
		s.observeIssuance(result, "issuance_failed", result.err.Error())
		s.send(ctx, turncontrol.Unavailable{Authority: s.header(), Profile: service.profile.Identity.Profile,
			RetryAfter: ServiceCooldown, Reason: result.err.Error()})
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
		return
	}
	s.observe(value, "credentials_delegated", "")
}

func (s *Sender) promote(ctx context.Context, service *service) {
	pending := service.pending
	if pending == nil || !pending.readyLane.Valid() {
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
	// Admission revocation and promotion share one commit boundary. Checking the
	// channel's terminal signal also covers delayed attempt cleanup callbacks.
	s.mu.Lock()
	admission := pending.admission
	if admission == nil || admission.lane != pending.readyLane || !admission.live() || pending.recovery != 0 {
		s.mu.Unlock()
		return
	}
	old := service.current
	service.current, service.pending = pending, nil
	s.mu.Unlock()
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
	s.applyReport(value)
	s.send(ctx, turncontrol.Retire{Authority: value.grant.Authority, PathID: value.grant.PathID, Reason: reason})
	if s.config.Retire != nil {
		s.config.Retire(value.grant.PathID)
	}
	value.lease.Close()
	s.observe(value, "path_retired", fmt.Sprint(reason))
}
func (s *Sender) close() {
	s.controls.close()
	s.mu.Lock()
	clear(s.paths)
	s.mu.Unlock()
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
	if s.owner == ([16]byte{}) || len(s.services) == 0 && s.advertisedRevision == 0 {
		return
	}
	profiles := make([]string, 0, len(s.services))
	for _, service := range s.services {
		profiles = append(profiles, service.profile.Identity.Profile)
	}
	if s.send(ctx, turncontrol.Available{Authority: turncontrol.Header{
		SessionID: s.config.SessionID, OwnerPathID: s.owner, Sequence: s.inventory.Revision,
	}, Profiles: profiles}) {
		s.advertisedRevision = s.inventory.Revision
	}
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
