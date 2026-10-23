package turnpath

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/connectionpath"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type ReceiverPath interface {
	Lane() (turncontrol.Lane, bool)
	Done() <-chan struct{}
	Close() error
}
type ReceiverConfig struct {
	SessionID         [16]byte
	Send              func(context.Context, []byte) error
	Open              func(context.Context, turncontrol.Grant, iceconfig.Connection) (ReceiverPath, error)
	Admitted          func(turncontrol.Grant, ReceiverPath)
	Observe           func(Event)
	Now               func() time.Time
	Tick              <-chan time.Time
	Measure           func(turncontrol.Lane) Measurement
	NetworkGeneration func() [16]byte
}
type Measurement struct {
	UsefulBytes  uint64
	Unhelpful    bool
	BusyTime     time.Duration
	PendingBytes uint64
}
type receiverPath struct {
	grant              turncontrol.Grant
	path               ReceiverPath
	expires            time.Time
	observedLane       turncontrol.Lane
	sentReadyLane      turncontrol.Lane
	reported           uint64
	accountedLaneBytes uint64
	sampleSequence     uint64
}
type Receiver struct {
	config               ReceiverConfig
	messages             *controlMailbox
	demands              chan bool
	mu                   sync.Mutex
	active               bool
	paths                map[[16]byte]*receiverPath
	retired              map[[16]byte]time.Time
	owner                [16]byte
	profiles             map[string]bool
	sequence, activation uint64
	inventoryRevision    uint64
	refresh              time.Time
	recoveries           map[[16]byte]turncontrol.RecoveryReason
}

func NewReceiver(config ReceiverConfig) *Receiver {
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Receiver{config: config, messages: newControlMailbox(), demands: make(chan bool, 1),
		paths: make(map[[16]byte]*receiverPath), retired: make(map[[16]byte]time.Time), recoveries: make(map[[16]byte]turncontrol.RecoveryReason)}
}
func (r *Receiver) Handle(ctx context.Context, body []byte) error {
	message, err := turncontrol.Decode(body)
	if err != nil || message.Header().SessionID != r.config.SessionID {
		return turncontrol.ErrMessage
	}
	switch message.(type) {
	case turncontrol.Available, turncontrol.Grant, turncontrol.Retire, turncontrol.Unavailable:
		return r.messages.put(ctx, message)
	default:
		return turncontrol.ErrMessage
	}
}
func (r *Receiver) SetDemand(active bool) {
	r.mu.Lock()
	r.active = active
	r.mu.Unlock()
	select {
	case r.demands <- true:
	default:
	}
}
func (r *Receiver) contentDemand() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}
func (r *Receiver) Run(ctx context.Context) {
	tick := r.config.Tick
	if tick == nil {
		ticker := time.NewTicker(MaintenanceInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	defer func() {
		r.messages.close()
		for _, entry := range r.paths {
			_ = entry.path.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.messages.wake:
			if message, ok := r.messages.take(); ok {
				r.apply(ctx, message)
			}
		case <-r.demands:
			r.maintain(ctx)
		case <-tick:
			r.maintain(ctx)
		}
	}
}
func (r *Receiver) apply(ctx context.Context, message turncontrol.Message) {
	h := message.Header()
	if r.owner != ([16]byte{}) && h.OwnerPathID != r.owner {
		return
	}
	switch value := message.(type) {
	case turncontrol.Available:
		// Inventory revisions are independent of receiver demand sequences.
		// Replays cannot restore a removed service or roll back configuration.
		if h.Sequence <= r.inventoryRevision {
			return
		}
		r.inventoryRevision = h.Sequence
		r.owner = h.OwnerPathID
		r.profiles = make(map[string]bool)
		for _, profile := range value.Profiles {
			r.profiles[profile] = true
		}
		for _, path := range r.paths {
			if !r.profiles[path.grant.Profile] {
				r.retire(ctx, path, turncontrol.ProviderRevoked)
			}
		}
		r.maintain(ctx)
	case turncontrol.Grant:
		r.acceptGrant(ctx, value)
	case turncontrol.Retire:
		if path := r.paths[value.PathID]; path != nil && path.grant.Authority == h {
			r.retire(ctx, path, value.Reason)
		}
	case turncontrol.Unavailable:
		if r.profiles[value.Profile] {
			r.observe(turncontrol.Grant{Authority: h, Profile: value.Profile}, connectionpath.MilestoneUnavailable, value.Reason)
		}
	}
}

func (r *Receiver) acceptsGrant(value turncontrol.Grant) bool {
	h := value.Authority
	if !r.contentDemand() || !r.profiles[value.Profile] || h.Sequence < r.activation ||
		h.Sequence > r.sequence || r.paths[value.PathID] != nil || len(r.paths) >= MaximumActiveServices+MaximumReplacementPaths {
		return false
	}
	if until, retired := r.retired[value.PathID]; retired && r.config.Now().Before(until) {
		return false
	}
	if value.Replaces != ([16]byte{}) {
		old := r.paths[value.Replaces]
		return old != nil && old.grant.Profile == value.Profile && old.grant.Account == value.Account && old.grant.Provider == value.Provider
	}
	services := make(map[string]bool)
	for _, path := range r.paths {
		services[path.grant.Profile] = true
	}
	return !services[value.Profile] && len(services) < MaximumActiveServices
}

func (r *Receiver) acceptGrant(ctx context.Context, value turncontrol.Grant) {
	if !r.acceptsGrant(value) {
		return
	}
	credential, err := iceconfig.Fixed(value.Username, value.Password)
	var expires time.Time
	if value.CredentialKind == turncontrol.Expiring {
		expires = r.config.Now().Add(value.ValidFor)
		credential, err = iceconfig.Expiring(value.Username, value.Password, expires)
	}
	if err != nil {
		return
	}
	relay, err := iceconfig.NewRelay(iceconfig.Identity{Provider: value.Provider, Account: value.Account, Profile: value.Profile}, value.URLs, credential)
	if err != nil {
		return
	}
	connection, err := iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	if err != nil {
		return
	}
	path, err := r.config.Open(ctx, value, connection)
	if err != nil {
		r.send(ctx, turncontrol.Retire{Authority: value.Authority, PathID: value.PathID, Reason: turncontrol.Unhelpful})
		r.observe(value, connectionpath.MilestoneUnavailable, err.Error())
		return
	}
	r.paths[value.PathID] = &receiverPath{grant: value, path: path, expires: expires}
	r.observe(value, connectionpath.MilestoneConnecting, "")
}

func (r *Receiver) maintain(ctx context.Context) {
	now := r.config.Now()
	active := r.contentDemand()
	if !active {
		for _, path := range r.paths {
			r.measure(ctx, path)
		}
	}
	r.refreshDemand(ctx, now, active)
	r.requestRecoveries(ctx)
	for _, path := range r.paths {
		reason := path.retirementReason(now, active)
		if reason == 0 {
			r.notifyReady(ctx, path)
			if r.measure(ctx, path).Unhelpful {
				reason = turncontrol.Unhelpful
			}
		}
		if reason != 0 {
			r.measure(ctx, path)
			r.send(ctx, turncontrol.Retire{Authority: path.grant.Authority, PathID: path.grant.PathID, Reason: reason})
			r.retire(ctx, path, reason)
		}
	}
}

func (r *Receiver) refreshDemand(ctx context.Context, now time.Time, active bool) {
	if r.owner != ([16]byte{}) && (r.refresh.IsZero() && active || !r.refresh.IsZero() && (!active || !now.Before(r.refresh))) {
		r.sequence++
		hold := time.Duration(0)
		if active {
			hold = turncontrol.MaximumDemandLifetime
			if r.refresh.IsZero() {
				r.activation = r.sequence
			}
			r.refresh = now.Add(hold / 2)
		} else {
			r.refresh = time.Time{}
			r.activation = r.sequence + 1
		}
		r.send(ctx, turncontrol.Demand{Authority: turncontrol.Header{SessionID: r.config.SessionID, OwnerPathID: r.owner, Sequence: r.sequence}, HoldFor: hold})
	}
}

func (path *receiverPath) retirementReason(now time.Time, active bool) turncontrol.RetireReason {
	switch {
	case !active:
		return turncontrol.Idle
	case !path.expires.IsZero() && !now.Before(path.expires):
		return turncontrol.Expired
	default:
		select {
		case <-path.path.Done():
			return turncontrol.Unhelpful
		default:
			return 0
		}
	}
}

func (r *Receiver) notifyReady(ctx context.Context, path *receiverPath) {
	lane, live := path.path.Lane()
	if !live || !lane.Valid() || signalClosed(path.path.Done()) {
		return
	}
	if path.observedLane != lane {
		path.observedLane = lane
		path.accountedLaneBytes = 0
		if r.config.Admitted != nil {
			r.config.Admitted(path.grant, path.path)
		}
		r.observe(path.grant, connectionpath.MilestoneReady, "")
	}
	if path.sentReadyLane != lane && r.send(ctx, turncontrol.Ready{Authority: path.grant.Authority, PathID: path.grant.PathID, Lane: lane}) {
		path.sentReadyLane = lane
	}
}

func (r *Receiver) measure(ctx context.Context, path *receiverPath) Measurement {
	if !path.observedLane.Valid() || r.config.Measure == nil {
		return Measurement{}
	}
	measurement := r.config.Measure(path.observedLane)
	if measurement.UsefulBytes > path.accountedLaneBytes {
		total := path.reported + measurement.UsefulBytes - path.accountedLaneBytes
		if r.send(ctx, turncontrol.Report{Authority: path.grant.Authority, PathID: path.grant.PathID, UsefulBytes: total}) {
			path.reported, path.accountedLaneBytes = total, measurement.UsefulBytes
		}
	}
	if measurement.BusyTime > 0 {
		var network [16]byte
		if r.config.NetworkGeneration != nil {
			network = r.config.NetworkGeneration()
		}
		path.sampleSequence++
		r.send(ctx, turncontrol.Quality{Authority: path.grant.Authority, PathID: path.grant.PathID,
			Lane: path.observedLane, NetworkGeneration: network, SampleSequence: path.sampleSequence,
			ContentBytes: measurement.UsefulBytes, BusyMilliseconds: uint64(measurement.BusyTime / time.Millisecond), PendingBytes: measurement.PendingBytes})
	}
	return measurement
}

func (r *Receiver) retire(ctx context.Context, path *receiverPath, reason turncontrol.RetireReason) {
	r.measure(ctx, path)
	delete(r.paths, path.grant.PathID)
	now := r.config.Now()
	for id, until := range r.retired {
		if !now.Before(until) {
			delete(r.retired, id)
		}
	}
	if len(r.retired) < MaximumRetiredPaths {
		r.retired[path.grant.PathID] = now.Add(turncontrol.MaximumDemandLifetime)
	}
	_ = path.path.Close()
	r.observe(path.grant, connectionpath.MilestoneRetired, fmt.Sprint(reason))
}
func (r *Receiver) send(ctx context.Context, message turncontrol.Message) bool {
	body, err := turncontrol.Encode(message)
	if err != nil || r.config.Send == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, ControlWriteBudget)
	defer cancel()
	return r.config.Send(ctx, body) == nil
}
func (r *Receiver) observe(grant turncontrol.Grant, milestone connectionpath.Milestone, reason string) {
	slog.Debug("TURN receiver path", "provider", grant.Provider, "account", grant.Account, "profile", grant.Profile,
		"session", fmt.Sprintf("%x", r.config.SessionID), "path", fmt.Sprintf("%x", grant.PathID), "milestone", milestone.String(), "reason", reason)
	if r.config.Observe != nil {
		r.config.Observe(Event{At: r.config.Now(), Subject: Subject{SessionID: r.config.SessionID, PathID: grant.PathID}, Provider: grant.Provider, Account: grant.Account, Profile: grant.Profile, Milestone: milestone, Reason: reason})
	}
}
