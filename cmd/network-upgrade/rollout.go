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

var errDeferred = errors.New("node is still draining, upgrade deferred")

type maintenanceTarget interface {
	policy(context.Context, string) (drainStatus, error)
	local(context.Context, string, ...string) error
	verify(context.Context) error
}

type rollout struct {
	maintenanceTarget
	wait func(context.Context, time.Duration) error
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
	drain, cancel := context.WithTimeout(ctx, drainDeadline)
	start, err := r.enter(drain, status)
	cancel()
	restore := context.WithoutCancel(ctx)
	if err != nil {
		log.Info("node remains on current release", "error", err)
		return errors.Join(err, r.release(restore, log, time.Now().Add(releaseBudget)))
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

func (r rollout) enter(ctx context.Context, status drainStatus) (time.Time, error) {
	ticks := time.NewTicker(statusPoll)
	defer ticks.Stop()
	for {
		if !status.Held {
			return time.Time{}, errors.New("maintenance ownership changed")
		}
		if status.Confirmed {
			start := time.Now()
			attempt, cancel := context.WithTimeout(ctx, enterTimeout)
			err := r.local(attempt, "enter", strconv.FormatUint(status.Version, 10), strconv.FormatInt(reconnectBudget.Milliseconds(), 10))
			cancel()
			if err == nil {
				return start, nil
			}
			if !busy(err) {
				return time.Time{}, err
			}
		}
		select {
		case <-ctx.Done():
			return time.Time{}, errors.Join(errDeferred, ctx.Err())
		case <-ticks.C:
		}
		var err error
		if status, err = r.policy(ctx, "status"); err != nil {
			return time.Time{}, err
		}
	}
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
