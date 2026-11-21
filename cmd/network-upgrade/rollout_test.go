package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"
)

type deferredExit struct{}

func (deferredExit) Error() string { return "sessions still active" }
func (deferredExit) ExitCode() int { return busyExit }

type fakeTarget struct {
	calls       []string
	fail        map[string]error
	onCall      func(string)
	onStep      func(context.Context, string) error
	held        bool
	unconfirmed bool
	version     uint64
	recovery    recoveryState
}

func (f *fakeTarget) step(ctx context.Context, action string) error {
	f.calls = append(f.calls, action)
	if f.onCall != nil {
		f.onCall(action)
	}
	if f.onStep != nil {
		if err := f.onStep(ctx, action); err != nil {
			return err
		}
	}
	return f.fail[action]
}
func (f *fakeTarget) policy(ctx context.Context, action string) (drainStatus, error) {
	err := f.step(ctx, "authority "+action)
	if action == "release" && err == nil {
		f.held = false
	}
	return drainStatus{Held: f.held, Confirmed: !f.unconfirmed, Version: f.version}, err
}
func (f *fakeTarget) local(ctx context.Context, action string, args ...string) error {
	if action == "enter" && !slices.Equal(args, []string{strconv.FormatUint(f.version, 10), strconv.FormatInt(reconnectBudget.Milliseconds(), 10)}) {
		return errors.New("incorrect fence version or reconnect budget")
	}
	return f.step(ctx, "host "+action)
}
func (f *fakeTarget) localRecovery(ctx context.Context) (recoveryState, error) {
	if err := f.step(ctx, "host recover"); err != nil {
		return "", err
	}
	if f.recovery != "" {
		return f.recovery, nil
	}
	return recoveryHeld, nil
}
func (f *fakeTarget) verify(ctx context.Context) error { return f.step(ctx, "probe") }

func TestUpgradeOrdersSwitchVerificationAndRelease(t *testing.T) {
	failure := errors.New("unusable binary")
	cases := []struct {
		name   string
		fail   map[string]error
		want   []string
		failed bool
	}{
		{"verified candidate", nil, []string{"authority drain", "host enter", "host switch", "probe", "host release", "authority release"}, false},
		{"verified rollback", map[string]error{"host switch": failure}, []string{"authority drain", "host enter", "host switch", "host rollback", "probe", "host release", "authority release"}, true},
		{"failed rollback stays fenced", map[string]error{"host switch": failure, "host rollback": failure}, []string{"authority drain", "host enter", "host switch", "host rollback"}, true},
		{"drain reply lost", map[string]error{"authority drain": failure}, []string{"authority drain", "authority release"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTarget{held: true, version: 73, fail: tc.fail}
			err := (rollout{maintenanceTarget: f}).upgrade(t.Context(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if (err != nil) != tc.failed || !slices.Equal(f.calls, tc.want) {
				t.Fatalf("error=%v, calls=%v", err, f.calls)
			}
		})
	}
}

func TestBusyFenceDefersWithoutRestartAndFailedCandidateProbeRollsBack(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("host enter", func(t *testing.T) {
		f := &fakeTarget{held: true, version: 71, fail: map[string]error{"host enter": deferredExit{}}}
		err := (rollout{maintenanceTarget: f, drainTimeout: 15 * time.Millisecond}).upgrade(t.Context(), discard)
		if !errors.Is(err, errDeferred) || f.held || slices.Contains(f.calls, "host switch") {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if want := []string{"authority drain", "host enter", "authority release"}; !slices.Equal(f.calls, want) {
			t.Fatalf("calls=%v, want=%v", f.calls, want)
		}
	})
	t.Run("probe", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := &fakeTarget{held: true, version: 71, fail: map[string]error{}}
		f.onCall = func(step string) {
			if step == "probe" {
				cancel()
				f.fail[step] = errors.New("candidate rejected probe")
			}
			if step == "host rollback" {
				delete(f.fail, "probe")
				f.onCall = nil
			}
		}
		err := (rollout{maintenanceTarget: f}).upgrade(ctx, discard)
		if err == nil || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if !slices.Equal(f.calls, []string{"authority drain", "host enter", "host switch", "probe", "host rollback", "probe", "host release", "authority release"}) {
			t.Fatalf("rollback: %v", f.calls)
		}
	})
	if reconnectBudget > time.Minute || redialBudget <= 0 {
		t.Fatal("maintenance can outlive sender grace")
	}
}

func TestUpgradeDeferralAndReleaseClassification(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("drain deferral after busy enter releases authority without ssh host release", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{
			held:    true,
			version: 72,
			fail: map[string]error{
				"host enter":   deferredExit{},
				"host release": errors.New("ssh connection reset"),
			},
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: 15 * time.Millisecond}).upgrade(t.Context(), discard)
		if !errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if want := []string{"authority drain", "host enter", "authority release"}; !slices.Equal(f.calls, want) {
			t.Fatalf("calls=%v, want=%v", f.calls, want)
		}
	})

	t.Run("drain deferral before confirmed status releases authority without host calls", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{
			held:        true,
			unconfirmed: true,
			version:     72,
			fail: map[string]error{
				"host release": errors.New("ssh connection reset"),
			},
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: 15 * time.Millisecond}).upgrade(t.Context(), discard)
		if !errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if want := []string{"authority drain", "authority release"}; !slices.Equal(f.calls, want) {
			t.Fatalf("calls=%v, want=%v", f.calls, want)
		}
	})

	t.Run("failed host release after ambiguous local enter keeps authority hold and returns fatal error", func(t *testing.T) {
		t.Parallel()
		releaseErr := errors.New("ssh connection reset")
		f := &fakeTarget{
			held:    true,
			version: 72,
			fail: map[string]error{
				"host release": releaseErr,
			},
		}
		f.onStep = func(ctx context.Context, step string) error {
			if step == "host enter" {
				<-ctx.Done()
				return errors.New("signal: killed")
			}
			return nil
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: 15 * time.Millisecond}).upgrade(t.Context(), discard)
		if err == nil || errors.Is(err, errDeferred) || !errors.Is(err, releaseErr) || !f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if want := []string{"authority drain", "host enter", "host release"}; !slices.Equal(f.calls, want) {
			t.Fatalf("calls=%v, want=%v", f.calls, want)
		}
	})

	t.Run("failed authority release after drain deferral returns fatal error", func(t *testing.T) {
		t.Parallel()
		releaseErr := errors.New("authority release: HTTP 503")
		f := &fakeTarget{
			held:    true,
			version: 72,
			fail: map[string]error{
				"host enter":        deferredExit{},
				"authority release": releaseErr,
			},
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: 15 * time.Millisecond}).upgrade(t.Context(), discard)
		if err == nil || errors.Is(err, errDeferred) || !errors.Is(err, releaseErr) || !f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("parent signal cancellation during drain wait is not deferred and still releases hold", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := &fakeTarget{held: true, version: 72, fail: map[string]error{"host enter": deferredExit{}}}
		f.onCall = func(step string) {
			if step == "host enter" {
				cancel()
			}
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: time.Second}).upgrade(ctx, discard)
		if !errors.Is(err, context.Canceled) || errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if want := []string{"authority drain", "host enter", "authority release"}; !slices.Equal(f.calls, want) {
			t.Fatalf("calls=%v, want=%v", f.calls, want)
		}
	})

	t.Run("drain deadline expiring during local enter defers when release succeeds", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{held: true, version: 72, fail: map[string]error{}}
		f.onStep = func(ctx context.Context, step string) error {
			if step == "host enter" {
				<-ctx.Done()
				// Match exec.CommandContext killing ssh with a non-DeadlineExceeded exit error.
				return errors.New("signal: killed")
			}
			return nil
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: 15 * time.Millisecond}).upgrade(t.Context(), discard)
		if !errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
		if want := []string{"authority drain", "host enter", "host release", "authority release"}; !slices.Equal(f.calls, want) {
			t.Fatalf("calls=%v, want=%v", f.calls, want)
		}
	})

	t.Run("parent signal cancellation during local enter is not deferred and still releases hold", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := &fakeTarget{held: true, version: 72, fail: map[string]error{}}
		f.onStep = func(stepCtx context.Context, step string) error {
			if step == "host enter" {
				cancel()
				<-stepCtx.Done()
				return errors.New("signal: killed")
			}
			return nil
		}
		err := (rollout{maintenanceTarget: f, drainTimeout: time.Second}).upgrade(ctx, discard)
		if !errors.Is(err, context.Canceled) || errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("drain deadline expiring during authority status defers when release succeeds", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status func(context.Context) error
		}{
			{"ctx.Err", func(ctx context.Context) error { return ctx.Err() }},
			{"context.Cause", context.Cause},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := &fakeTarget{held: true, version: 72, fail: map[string]error{"host enter": deferredExit{}}}
				f.onStep = func(ctx context.Context, step string) error {
					if step == "authority status" {
						<-ctx.Done()
						return tc.status(ctx)
					}
					return nil
				}
				r := rollout{
					maintenanceTarget: f,
					drainTimeout:      15 * time.Millisecond,
					wait:              func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
				}
				err := r.upgrade(t.Context(), discard)
				if !errors.Is(err, errDeferred) || f.held {
					t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
				}
			})
		}
	})

	t.Run("enterTimeout expiring before drainDeadline returns fatal error", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{held: true, version: 72, fail: map[string]error{}}
		f.onStep = func(ctx context.Context, step string) error {
			if step == "host enter" {
				<-ctx.Done()
				return errors.New("signal: killed")
			}
			return nil
		}
		r := rollout{
			maintenanceTarget: f,
			drainTimeout:      time.Second,
			attemptTimeout:    15 * time.Millisecond,
		}
		err := r.upgrade(t.Context(), discard)
		if err == nil || errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("non-timeout authority status failure returns fatal error", func(t *testing.T) {
		t.Parallel()
		statusErr := errors.New("authority status: HTTP 500")
		f := &fakeTarget{
			held:    true,
			version: 72,
			fail: map[string]error{
				"host enter":       deferredExit{},
				"authority status": statusErr,
			},
		}
		r := rollout{
			maintenanceTarget: f,
			drainTimeout:      time.Second,
			wait:              func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		}
		err := r.upgrade(t.Context(), discard)
		if !errors.Is(err, statusErr) || errors.Is(err, errDeferred) || f.held {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})
}

func TestSwitchAndReleaseRetryBusyLock(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("rollback retries busy lock after cancelled switch", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rollbackBusy := 1
		f := &fakeTarget{held: true, version: 74, fail: map[string]error{}}
		f.onCall = func(step string) {
			switch step {
			case "host switch":
				cancel()
				f.fail[step] = ctx.Err()
			case "host rollback":
				if rollbackBusy > 0 {
					rollbackBusy--
					f.fail[step] = deferredExit{}
				} else {
					delete(f.fail, step)
				}
			}
		}
		err := (rollout{maintenanceTarget: f}).upgrade(ctx, discard)
		want := []string{"authority drain", "host enter", "host switch", "host rollback", "host rollback", "probe", "host release", "authority release"}
		if err == nil || f.held || !slices.Equal(f.calls, want) {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("recover inspect retries busy lock without rolling back", func(t *testing.T) {
		t.Parallel()
		inspectBusy := 1
		f := &fakeTarget{held: true, version: 75, fail: map[string]error{}}
		f.onCall = func(step string) {
			if step == "host inspect" {
				if inspectBusy > 0 {
					inspectBusy--
					f.fail[step] = deferredExit{}
				} else {
					delete(f.fail, step)
				}
			}
		}
		err := (rollout{maintenanceTarget: f}).recover(t.Context(), discard)
		want := []string{"authority status", "host recover", "host inspect", "host inspect", "probe", "host release", "authority release"}
		if err != nil || f.held || !slices.Equal(f.calls, want) {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("recover retries busy lock before closing unheld operation", func(t *testing.T) {
		t.Parallel()
		recoverBusy := 1
		f := &fakeTarget{
			held:     true,
			version:  76,
			recovery: recoveryReleased,
			fail: map[string]error{
				"host inspect":  errors.New("selected release is not running"),
				"host rollback": errors.New("operation does not own the local hold"),
				"probe":         errors.New("probe timed out"),
			},
		}
		f.onCall = func(step string) {
			if step == "host recover" {
				if recoverBusy > 0 {
					recoverBusy--
					f.fail[step] = deferredExit{}
				} else {
					delete(f.fail, step)
				}
			}
		}
		err := (rollout{maintenanceTarget: f}).recover(t.Context(), discard)
		want := []string{"authority status", "host recover", "host recover", "authority release"}
		if err != nil || f.held || !slices.Equal(f.calls, want) {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("recover fails closed without rollback when host recovery reports an error", func(t *testing.T) {
		t.Parallel()
		missingHold := errors.New("local hold missing for switched release")
		f := &fakeTarget{
			held:    true,
			version: 77,
			fail:    map[string]error{"host recover": missingHold},
		}
		err := (rollout{maintenanceTarget: f}).recover(t.Context(), discard)
		want := []string{"authority status", "host recover"}
		if !errors.Is(err, missingHold) || !f.held || !slices.Equal(f.calls, want) {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("recover fails closed on unexpected host recovery state", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{
			held:     true,
			version:  78,
			recovery: recoveryState("unexpected"),
		}
		err := (rollout{maintenanceTarget: f}).recover(t.Context(), discard)
		want := []string{"authority status", "host recover"}
		if err == nil || !f.held || !slices.Equal(f.calls, want) {
			t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
		}
	})

	t.Run("switchAndVerify bounds busy retries to stage deadline", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{fail: map[string]error{"host rollback": deferredExit{}}}
		err := (rollout{maintenanceTarget: f}).switchAndVerify(t.Context(), discard, "rollback", time.Now().Add(50*time.Millisecond))
		if !busy(err) || !errors.Is(err, context.DeadlineExceeded) || slices.Contains(f.calls, "probe") {
			t.Fatalf("error=%v, calls=%v", err, f.calls)
		}
	})

	t.Run("release bounds busy retries and preserves busy cause", func(t *testing.T) {
		t.Parallel()
		f := &fakeTarget{held: true, fail: map[string]error{"host release": deferredExit{}}}
		err := (rollout{maintenanceTarget: f}).release(t.Context(), discard, time.Now().Add(50*time.Millisecond))
		if !busy(err) || !errors.Is(err, context.DeadlineExceeded) || slices.Contains(f.calls, "authority release") {
			t.Fatalf("error=%v, calls=%v", err, f.calls)
		}
	})
}

func TestSwitchAndVerifyBacksOffAndCapsProbeAttempts(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	probeErr := errors.New("candidate leaked share before stop")
	f := &fakeTarget{held: true, version: 81, fail: map[string]error{"probe": probeErr}}
	var delays []time.Duration
	r := rollout{
		maintenanceTarget: f,
		wait: func(ctx context.Context, d time.Duration) error {
			delays = append(delays, d)
			return ctx.Err()
		},
	}

	err := r.switchAndVerify(t.Context(), discard, "switch", time.Now().Add(switchBudget))
	if !errors.Is(err, probeErr) {
		t.Fatalf("expected wrapped probe error, got %v", err)
	}
	wantDelays := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		4 * time.Second,
		4 * time.Second,
	}
	if !slices.Equal(delays, wantDelays) {
		t.Fatalf("delays=%v, want=%v", delays, wantDelays)
	}
	var probes int
	for _, call := range f.calls {
		if call == "probe" {
			probes++
		}
	}
	if probes != probeMaxAttempts {
		t.Fatalf("probe attempts=%d, want=%d", probes, probeMaxAttempts)
	}
	var totalDelay time.Duration
	for _, d := range delays {
		totalDelay += d
	}
	if totalDelay >= switchBudget {
		t.Fatalf("total backoff %v must fit within switchBudget %v", totalDelay, switchBudget)
	}
}
