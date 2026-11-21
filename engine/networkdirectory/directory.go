// Package networkdirectory owns signed network snapshots, bounded refresh and
// durable rollback protection. It never imports built-in infrastructure.
package networkdirectory

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/nodedirectory"
)

const (
	RefreshTimeout        = 6 * time.Second
	MirrorTimeout         = 2 * time.Second
	RefreshInterval       = 6 * time.Hour
	MinimumBackoff        = 30 * time.Second
	MaximumBackoff        = 30 * time.Minute
	MaximumTriggerBackoff = 5 * time.Minute
	ExplicitRetryCooldown = MinimumBackoff
	MaximumMirrors        = 8
	MirrorsPerRefresh     = 3
	maxBackoffShift       = 10

	refreshStageFetch    = "fetch"
	refreshStageVerify   = "verify"
	refreshStageInstall  = "install"
	refreshStageAccepted = "accepted"

	mirrorTierLive     = "live"
	mirrorTierFallback = "fallback"
)

type Config struct {
	Network         string
	Roots           []nodedirectory.Root
	Embedded        []byte
	Mirrors         []string
	FallbackMirrors []string
	Cache           Cache
	Fetch           func(context.Context, string) ([]byte, error)
	Now             func() time.Time
	Order           func(int) []int
	Diagnostics     diagnostics.Sink
}

type Directory struct {
	config            Config
	mu                sync.Mutex
	current           nodedirectory.Snapshot
	highest           nodedirectory.Cursor
	refreshing        *refreshCycle
	nextRefresh       time.Time
	earliestTrigger   time.Time
	explicitRetryAt   time.Time
	networkGeneration uint64
	scheduleChanged   chan struct{}
	failures          uint
	unchangedTriggers uint
	triggerPending    bool
	lastError         error
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
}

func validMirrorURL(raw string) bool {
	address, err := url.Parse(raw)
	return err == nil && address.Scheme == "https" && address.Host != "" && address.User == nil && address.Fragment == ""
}

func New(config Config) (*Directory, error) {
	if config.Network == "" || len(config.Mirrors)+len(config.FallbackMirrors) > MaximumMirrors {
		return nil, blocked(InvalidConfiguration, nodedirectory.ErrFormat)
	}
	for _, mirror := range config.Mirrors {
		if !validMirrorURL(mirror) {
			return nil, blocked(InvalidConfiguration, nodedirectory.ErrFormat)
		}
	}
	for _, mirror := range config.FallbackMirrors {
		if !validMirrorURL(mirror) {
			return nil, blocked(InvalidConfiguration, nodedirectory.ErrFormat)
		}
	}
	config.Roots = slices.Clone(config.Roots)
	for index := range config.Roots {
		config.Roots[index].Key = slices.Clone(config.Roots[index].Key)
	}
	config.Embedded = slices.Clone(config.Embedded)
	config.Mirrors = slices.Clone(config.Mirrors)
	config.FallbackMirrors = slices.Clone(config.FallbackMirrors)
	if config.Cache == nil {
		config.Cache = &MemoryCache{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Fetch == nil {
		config.Fetch = fetch
	}
	if config.Order == nil {
		config.Order = rand.Perm
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := &Directory{config: config, ctx: ctx, cancel: cancel, done: make(chan struct{}), scheduleChanged: make(chan struct{}, 1)}
	record, err := config.Cache.Load(ctx)
	if err != nil {
		cancel()
		return nil, blocked(TrustStateUnreadable, err)
	}
	if len(record.Envelope) > 0 {
		snapshot, err := nodedirectory.Verify(record.Envelope, config.Network, config.Roots)
		if err != nil || snapshot.Cursor() != record.Cursor {
			cancel()
			return nil, blocked(TrustStateInvalid, errors.Join(nodedirectory.ErrRollback, err))
		}
		directory.current, directory.highest = snapshot, record.Cursor
	} else if record.Cursor.Generation != 0 {
		cancel()
		return nil, blocked(TrustStateInvalid, nodedirectory.ErrRollback)
	}
	if len(config.Embedded) > 0 {
		// An expired embedded export is still verifiable history. It cannot
		// displace a newer cached generation, even after that cache expires.
		snapshot, err := nodedirectory.Verify(config.Embedded, config.Network, config.Roots)
		if err != nil {
			cancel()
			return nil, blocked(InvalidConfiguration, err)
		}
		if err := directory.commit(ctx, snapshot); err != nil && !errors.Is(err, nodedirectory.ErrRollback) {
			cancel()
			return nil, err
		}
	}
	go directory.run()
	return directory, nil
}

func (directory *Directory) Snapshot() (nodedirectory.Snapshot, bool) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return directory.current, directory.current.Usable(directory.config.Now(), nodedirectory.DefaultGrace)
}

func (directory *Directory) install(ctx context.Context, next nodedirectory.Snapshot) error {
	if !next.Usable(directory.config.Now(), nodedirectory.DefaultGrace) {
		return nodedirectory.ErrExpired
	}
	return directory.commit(ctx, next)
}

func (directory *Directory) commit(ctx context.Context, next nodedirectory.Snapshot) error {
	var rejected error
	record, err := directory.config.Cache.Update(ctx, func(previous Record) (Record, error) {
		if err := nodedirectory.Compare(previous.Cursor, next.Cursor()); err != nil {
			rejected = err
			return Record{}, err
		}
		aliases, err := nodedirectory.RetainAliases(previous.Aliases, next)
		if err != nil {
			rejected = err
			return Record{}, err
		}
		return Record{Cursor: next.Cursor(), Envelope: next.Envelope(), Aliases: aliases}, nil
	})
	if err != nil {
		if rejected != nil {
			return rejected
		}
		return blocked(TrustStateUnwritable, err)
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if err := nodedirectory.Compare(directory.highest, next.Cursor()); err != nil {
		return err
	}
	directory.current, directory.highest = next, record.Cursor
	return nil
}

// Refresh coalesces user retries, unknown-alias resolution and periodic refresh.
// Caller cancellation stops waiting, not an update another task also needs.
func (directory *Directory) Refresh(ctx context.Context) error {
	return directory.requestRefresh(ctx, false, nil)
}

// TriggerRefresh advances periodic maintenance after an all-cooled plan or
// admission refusal. Bursts coalesce into one fetch, and repeated triggered
// fetches that observe an unchanged cursor back off up to MaximumTriggerBackoff.
func (directory *Directory) TriggerRefresh() {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.refreshing != nil || directory.failures != 0 || !directory.earliestTrigger.Before(directory.nextRefresh) {
		return
	}
	directory.nextRefresh = directory.earliestTrigger
	directory.triggerPending = true
	directory.notifySchedule()
}

// NetworkChanged invalidates failure and unchanged-trigger backoff accumulated
// on a previous network generation so recovery does not wait behind stale outages.
func (directory *Directory) NetworkChanged() {
	directory.mu.Lock()
	if directory.ctx.Err() != nil || len(directory.config.Roots) == 0 || len(directory.config.Mirrors)+len(directory.config.FallbackMirrors) == 0 {
		directory.mu.Unlock()
		return
	}
	directory.networkGeneration++
	now := directory.config.Now()
	directory.unchangedTriggers = 0
	if earliest := now.Add(MinimumBackoff); directory.earliestTrigger.After(earliest) {
		directory.earliestTrigger = earliest
		if directory.triggerPending && directory.nextRefresh.After(earliest) {
			directory.nextRefresh = earliest
			directory.notifySchedule()
		}
	}
	previousFailures := directory.failures
	if previousFailures == 0 {
		directory.mu.Unlock()
		return
	}
	directory.failures = 0
	directory.triggerPending = false
	directory.explicitRetryAt = time.Time{}
	directory.nextRefresh = now
	// Sink.Emit is strictly nonblocking; emit under mu so Close stays ordered after publication.
	directory.emitBackoffReset(now, previousFailures)
	directory.notifySchedule()
	directory.mu.Unlock()
}

func (directory *Directory) notifySchedule() {
	select {
	case directory.scheduleChanged <- struct{}{}:
	default:
	}
}

func (directory *Directory) emitBackoffReset(now time.Time, previousFailures uint) {
	if !directory.config.Diagnostics.Enabled() {
		return
	}
	directory.config.Diagnostics.Emit(diagnostics.New("network_directory_backoff_reset", diagnostics.Fields{
		"network":           diagnostics.Text(directory.config.Network),
		"previous_failures": diagnostics.Uint(uint64(previousFailures)),
		"reason":            diagnostics.Text("network_changed"),
	}).At(now))
}

func (directory *Directory) emitRefreshDeferred(now time.Time, err *UnavailableError) {
	if !directory.config.Diagnostics.Enabled() {
		return
	}
	fields := diagnostics.Fields{
		"network": diagnostics.Text(directory.config.Network),
		"reason":  diagnostics.Text(string(err.Reason)),
	}
	if !err.RetryAt.IsZero() {
		fields["retry_at"] = diagnostics.Text(err.RetryAt.UTC().Format(time.RFC3339Nano))
	}
	directory.config.Diagnostics.Emit(diagnostics.New("network_directory_refresh_deferred", fields).At(now))
}

func (directory *Directory) emitRefreshCompleted(now time.Time, cursor nodedirectory.Cursor, failures, unchangedTriggers uint, nextRefresh, earliestTrigger, explicitRetryAt time.Time, err error) {
	if !directory.config.Diagnostics.Enabled() {
		return
	}
	fields := diagnostics.Fields{
		"network":      diagnostics.Text(directory.config.Network),
		"generation":   diagnostics.Uint(cursor.Generation),
		"serial":       diagnostics.Uint(cursor.Serial),
		"failures":     diagnostics.Uint(uint64(failures)),
		"next_refresh": diagnostics.Text(nextRefresh.UTC().Format(time.RFC3339Nano)),
	}
	if err == nil && unchangedTriggers > 0 {
		fields["unchanged_triggers"] = diagnostics.Uint(uint64(unchangedTriggers))
		fields["earliest_trigger"] = diagnostics.Text(earliestTrigger.UTC().Format(time.RFC3339Nano))
	}
	if !explicitRetryAt.IsZero() {
		fields["retry_at"] = diagnostics.Text(explicitRetryAt.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		fields["error"] = diagnostics.Text(err.Error())
	}
	directory.config.Diagnostics.Emit(diagnostics.New("network_directory_refresh_completed", fields).At(now))
}

func (directory *Directory) emitMirrorAttempt(now time.Time, mirror, tier, stage string, snapshot nodedirectory.Snapshot, duration time.Duration, err error) {
	if !directory.config.Diagnostics.Enabled() {
		return
	}
	cursor := snapshot.Cursor()
	fields := diagnostics.Fields{
		"network":     diagnostics.Text(directory.config.Network),
		"mirror":      diagnostics.Text(mirror),
		"tier":        diagnostics.Text(tier),
		"stage":       diagnostics.Text(stage),
		"generation":  diagnostics.Uint(cursor.Generation),
		"serial":      diagnostics.Uint(cursor.Serial),
		"duration_ms": diagnostics.Millis(max(duration, 0)),
	}
	if trustedUntil := snapshot.TrustedUntil(nodedirectory.DefaultGrace); !trustedUntil.IsZero() {
		fields["trusted_until"] = diagnostics.Text(trustedUntil.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		fields["error"] = diagnostics.Text(err.Error())
	}
	directory.config.Diagnostics.Emit(diagnostics.New("network_directory_refresh", fields).At(now))
}

func (directory *Directory) run() {
	defer close(directory.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-directory.ctx.Done():
			return
		case <-timer.C:
		case <-directory.scheduleChanged:
		}
		_ = directory.requestRefresh(directory.ctx, true, nil)
		directory.mu.Lock()
		// Reading the schedule consumes all changes published before this lock.
		select {
		case <-directory.scheduleChanged:
		default:
		}
		delay := max(0, directory.nextRefresh.Sub(directory.config.Now()))
		directory.mu.Unlock()
		timer.Reset(delay)
	}
}

func (directory *Directory) Close() {
	directory.mu.Lock()
	directory.cancel()
	refreshing := directory.refreshing
	directory.mu.Unlock()
	<-directory.done
	if refreshing != nil {
		<-refreshing.done
	}
}
