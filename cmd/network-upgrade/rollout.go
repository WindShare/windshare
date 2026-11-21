package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

const (
	drainDeadline   = 5 * time.Minute
	statusPoll      = 2 * time.Second
	busyExit        = 75
	enterTimeout    = 5 * time.Second
	switchBudget    = 18 * time.Second
	rollbackBudget  = 22 * time.Second
	releaseBudget   = 8 * time.Second
	redialBudget    = 10 * time.Second
	reconnectBudget = switchBudget + rollbackBudget + releaseBudget + redialBudget
	retryInterval   = 250 * time.Millisecond
	// Exponential backoff + cap spans startup recovery across switchBudget while
	// bounding synthetic shares a broken candidate can leak before rollback.
	probeMaxBackoff  = 4 * time.Second
	probeMaxAttempts = 8
)

var (
	errDeferred     = errors.New("node is still draining, upgrade deferred")
	errDrainExpired = errors.New("drain deadline expired before node became idle")
)

type maintenanceTarget interface {
	policy(context.Context, string) (drainStatus, error)
	local(context.Context, string, ...string) error
	verify(context.Context) error
}

type rollout struct {
	maintenanceTarget
	wait           func(context.Context, time.Duration) error
	drainTimeout   time.Duration
	attemptTimeout time.Duration
}

func timeoutOr(override, fallback time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return fallback
}

func isDrainExpired(parent, drain context.Context) bool {
	return parent.Err() == nil && errors.Is(context.Cause(drain), errDrainExpired)
}

func (r rollout) upgrade(ctx context.Context, log *slog.Logger) error {
	status, err := r.policy(ctx, "drain")
	if err != nil {
		// The hold may have committed before its response was lost.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseBudget)
		defer cancel()
		_, release := r.policy(cleanup, "release")
		return errors.Join(err, release)
	}
	log.Info("maintenance drain requested", "policy_version", status.Version)
	start, err := r.enter(ctx, status)
	restore := context.WithoutCancel(ctx)
	if err != nil {
		log.Info("node remains on current release", "error", err)
		if releaseErr := r.release(restore, log, time.Now().Add(releaseBudget)); releaseErr != nil {
			log.Error("maintenance hold release failed, hold may remain active", "error", releaseErr)
			return errors.Join(err, releaseErr)
		}
		// Only a cleanly released drain timeout may exit busyExit (75); any failed
		// cleanup leaves a fleet-blocking hold and must exit fatal (1).
		if errors.Is(err, errDrainExpired) {
			if ctx.Err() != nil {
				return errors.Join(err, ctx.Err())
			}
			return errDeferred
		}
		return err
	}
	log.Info("local admission fenced")
	return r.finish(ctx, log, start, "switch")
}

func (r rollout) recover(ctx context.Context, log *slog.Logger) error {
	status, err := r.policy(ctx, "status")
	if err != nil {
		return err
	}
	if !status.Held {
		return errors.New("operation no longer owns the authority drain")
	}
	return r.finish(ctx, log, time.Now(), "inspect")
}

func (r rollout) finish(ctx context.Context, log *slog.Logger, start time.Time, action string) error {
	restore := context.WithoutCancel(ctx)
	// Charge the enter round trip, both switches and cleanup to the same clock.
	// The node checked this window against every already-disconnected sender.
	candidateUntil := start.Add(switchBudget)
	rollbackUntil := candidateUntil.Add(rollbackBudget)
	err := r.switchAndVerify(ctx, log, action, candidateUntil)
	if err != nil {
		log.Warn("rolling back candidate", "error", err)
		if rollback := r.switchAndVerify(restore, log, "rollback", rollbackUntil); rollback != nil {
			return errors.Join(err, fmt.Errorf("rollback verification failed, maintenance hold retained: %w", rollback))
		}
		log.Info("previous release verified")
	} else {
		log.Info("selected release verified")
	}
	return errors.Join(err, r.release(restore, log, rollbackUntil.Add(releaseBudget)))
}

func (r rollout) enter(parent context.Context, status drainStatus) (time.Time, error) {
	// Wrap DeadlineExceeded in the timeout cause so HTTP clients returning either
	// ctx.Err() or context.Cause(ctx) stay distinguishable from non-timeout errors.
	drainCause := fmt.Errorf("%w: %w", errDrainExpired, context.DeadlineExceeded)
	drain, cancel := context.WithTimeoutCause(parent, timeoutOr(r.drainTimeout, drainDeadline), drainCause)
	defer cancel()
	for {
		if !status.Held {
			return time.Time{}, errors.New("maintenance ownership changed")
		}
		if status.Confirmed {
			start, entered, err := r.tryEnter(parent, drain, status.Version)
			if err != nil || entered {
				return start, err
			}
		}
		if err := r.sleep(drain, statusPoll); err != nil {
			if isDrainExpired(parent, drain) {
				return time.Time{}, errDrainExpired
			}
			return time.Time{}, err
		}
		var err error
		if status, err = r.policy(drain, "status"); err != nil {
			if isDrainExpired(parent, drain) && errors.Is(err, context.DeadlineExceeded) {
				return time.Time{}, errDrainExpired
			}
			return time.Time{}, err
		}
	}
}

func (r rollout) tryEnter(parent, drain context.Context, version uint64) (time.Time, bool, error) {
	start := time.Now()
	attempt, cancel := context.WithTimeout(drain, timeoutOr(r.attemptTimeout, enterTimeout))
	err := r.local(attempt, "enter", strconv.FormatUint(version, 10), strconv.FormatInt(reconnectBudget.Milliseconds(), 10))
	attemptErr := attempt.Err()
	cancel()
	if err == nil {
		return start, true, nil
	}
	if busy(err) {
		return time.Time{}, false, nil
	}
	if attemptErr != nil && isDrainExpired(parent, drain) {
		return time.Time{}, false, errDrainExpired
	}
	if parent.Err() != nil && !errors.Is(err, parent.Err()) {
		return time.Time{}, false, errors.Join(err, parent.Err())
	}
	return time.Time{}, false, err
}

func busy(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == busyExit
}

func (r rollout) sleep(ctx context.Context, d time.Duration) error {
	if r.wait != nil {
		return r.wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Retry exit 75 (host .lock or local admission gate contention) within the
// stage deadline; an interrupted switch may hold .lock until restart exits.
func (r rollout) localUntilReady(ctx context.Context, log *slog.Logger, action string) error {
	logged := false
	for {
		err := r.local(ctx, action)
		if err == nil || !busy(err) {
			return err
		}
		if !logged {
			log.Warn("host operation busy, retrying", "action", action, "error", err)
			logged = true
		}
		if waitErr := r.sleep(ctx, retryInterval); waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
}

func (r rollout) switchAndVerify(parent context.Context, log *slog.Logger, action string, until time.Time) error {
	ctx, cancel := context.WithDeadline(parent, until)
	defer cancel()
	if err := r.localUntilReady(ctx, log, action); err != nil {
		return err
	}
	backoff := retryInterval
	for attempt := 1; ; attempt++ {
		err := r.verify(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if attempt >= probeMaxAttempts {
			return fmt.Errorf("probe failed after %d attempts: %w", attempt, err)
		}
		log.Warn("probe attempt failed, backing off", "action", action, "attempt", attempt, "next_delay", backoff, "error", err)
		if waitErr := r.sleep(ctx, backoff); waitErr != nil {
			return errors.Join(err, waitErr)
		}
		backoff = min(backoff*2, probeMaxBackoff)
	}
}

func (r rollout) release(parent context.Context, log *slog.Logger, until time.Time) error {
	ctx, cancel := context.WithDeadline(parent, until)
	defer cancel()
	if err := r.localUntilReady(ctx, log, "release"); err != nil {
		return fmt.Errorf("release local maintenance hold: %w", err)
	}
	_, err := r.policy(ctx, "release")
	return err
}
