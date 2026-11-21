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
	calls   []string
	fail    map[string]error
	onCall  func(string)
	held    bool
	version uint64
}

func (f *fakeTarget) step(action string) error {
	f.calls = append(f.calls, action)
	if f.onCall != nil {
		f.onCall(action)
	}
	return f.fail[action]
}
func (f *fakeTarget) policy(_ context.Context, action string) (drainStatus, error) {
	err := f.step("authority " + action)
	if action == "release" {
		f.held = false
	}
	return drainStatus{Held: f.held, Confirmed: true, Version: f.version}, err
}
func (f *fakeTarget) local(_ context.Context, action string, args ...string) error {
	if action == "enter" && !slices.Equal(args, []string{strconv.FormatUint(f.version, 10), strconv.FormatInt(reconnectBudget.Milliseconds(), 10)}) {
		return errors.New("incorrect fence version or reconnect budget")
	}
	return f.step("host " + action)
}
func (f *fakeTarget) verify(context.Context) error { return f.step("probe") }

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
			err := (rollout{f}).upgrade(t.Context(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if (err != nil) != tc.failed || !slices.Equal(f.calls, tc.want) {
				t.Fatalf("error=%v, calls=%v", err, f.calls)
			}
		})
	}
}

func TestBusyFenceDefersWithoutRestartAndFailedCandidateProbeRollsBack(t *testing.T) {
	for _, action := range []string{"host enter", "probe"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &fakeTarget{held: true, version: 71, fail: map[string]error{}}
			f.onCall = func(step string) {
				if step == action {
					cancel()
					if action == "host enter" {
						f.fail[step] = deferredExit{}
					} else {
						f.fail[step] = errors.New("candidate rejected probe")
					}
				}
				if step == "host rollback" {
					delete(f.fail, "probe")
					f.onCall = nil
				}
			}
			err := (rollout{f}).upgrade(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || f.held {
				t.Fatalf("error=%v, held=%v, calls=%v", err, f.held, f.calls)
			}
			if action == "host enter" && (!errors.Is(err, errDeferred) || slices.Contains(f.calls, "host switch")) {
				t.Fatalf("busy fence: %v", f.calls)
			}
			if action == "probe" && !slices.Equal(f.calls, []string{"authority drain", "host enter", "host switch", "probe", "host rollback", "probe", "host release", "authority release"}) {
				t.Fatalf("rollback: %v", f.calls)
			}
		})
	}
	if reconnectBudget > time.Minute || redialBudget <= 0 {
		t.Fatal("maintenance can outlive sender grace")
	}
}
