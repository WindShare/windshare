package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestAppCommandSurfaceReportsActionableFailures(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "missing command", wantCode: ExitUsage, wantStderr: "Usage:"},
		{name: "help", args: []string{"help"}, wantCode: ExitOK, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"unknown"}, wantCode: ExitUsage, wantStderr: "unknown command"},
		{
			name:       "malformed share flag",
			args:       []string{"share", "--definitely-unknown"},
			wantCode:   ExitUsage,
			wantStderr: "unknown option",
		},
		{
			name:       "missing get link",
			args:       []string{"get"},
			wantCode:   ExitUsage,
			wantStderr: "exactly one link argument is required",
		},
		{
			name:       "malformed get flag",
			args:       []string{"get", "--definitely-unknown"},
			wantCode:   ExitUsage,
			wantStderr: "unknown option",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _, stderr := newSemanticTestApp(strings.NewReader(""))
			if code := app.Run(context.Background(), test.args); code != test.wantCode {
				t.Fatalf("exit=%d want=%d stderr=%q", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("stderr=%q does not contain %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestTopLevelHelpMatchesOrdinaryOutputCommandContracts(t *testing.T) {
	app, stdout, stderr := newSemanticTestApp(strings.NewReader(""))
	if code := app.Run(context.Background(), []string{"help"}); code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	output := stderr.String()
	for _, required := range []string{
		"wind get <link>",
		"wind get -o <directory> <link>",
		"incomplete",
		"resumable",
		"cleanup-pending",
		"item-blocked",
		"Published files and unknown objects are always preserved.",
	} {
		if !strings.Contains(output, required) {
			t.Fatalf("help=%q does not contain %q", output, required)
		}
	}
	for _, stale := range []string{"paused state", "live item", "resume cleanup"} {
		if strings.Contains(output, stale) {
			t.Fatalf("help=%q contains retired wording %q", output, stale)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestShareRequestValidationPreservesSuiteBoundaries(t *testing.T) {
	t.Run("valid explicit configuration", func(t *testing.T) {
		app, _, _ := newSemanticTestApp(strings.NewReader(""))
		request, parse := app.parseShareRequest([]string{
			"root", "--relay", "wss://relay.example", "--front-url", "https://app.example",
			"--block-size", "65536", "--split-key",
		})
		if parse != requestParseReady {
			t.Fatalf("parse=%d", parse)
		}
		if !reflect.DeepEqual(request.paths, []string{"root"}) ||
			request.relayURL != "wss://relay.example" ||
			request.frontURL != "https://app.example" ||
			request.chunkSize != 65536 ||
			!request.splitKey {
			t.Fatalf("request=%+v", request)
		}
	})

	for _, test := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "missing roots", wantStderr: "at least one path"},
		{name: "empty relay", args: []string{"root", "--relay", ""}, wantStderr: "relay URL"},
		{name: "empty frontend", args: []string{"root", "--front-url", ""}, wantStderr: "frontend URL"},
		{
			name:       "negative block size",
			args:       []string{"root", "--block-size", "-1"},
			wantStderr: "outside the suite-02 range",
		},
		{
			name: "block size exceeds wire width",
			args: []string{
				"root", "--block-size", strconv.FormatUint(uint64(math.MaxUint32)+1, 10),
			},
			wantStderr: "outside the suite-02 range",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, stderr := newSemanticTestApp(strings.NewReader(""))
			if _, parse := app.parseShareRequest(test.args); parse != requestParseUsageFailure {
				t.Fatalf("parse=%d want=%d", parse, requestParseUsageFailure)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("stderr=%q does not contain %q", stderr.String(), test.wantStderr)
			}
		})
	}

	t.Run("missing source fails before relay registration", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader(""))
		app.revisionCapacity = newTestRevisionCapacity(t)
		missing := filepath.Join(t.TempDir(), "missing")
		if code := app.Run(context.Background(), []string{"share", missing}); code != ExitUsage {
			t.Fatalf("exit=%d want=%d stderr=%q", code, ExitUsage, stderr.String())
		}
		if !strings.Contains(stderr.String(), "unexpected error") || strings.Contains(stderr.String(), missing) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}

func TestGetCapabilityInputStaysLocalAndUnambiguous(t *testing.T) {
	capability := newSemanticCapability(t, "wss://relay.example")
	bare, keyString, err := capability.SplitURL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("explicit key", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader("ignored"))
		resolved, err := app.resolveLink(bare, keyString)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resolved, capability) {
			t.Fatalf("resolved=%+v want=%+v", resolved, capability)
		}
		if stderr.Len() != 0 {
			t.Fatalf("explicit key unexpectedly prompted: %q", stderr.String())
		}
	})

	t.Run("interactive key", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader(keyString + "\n"))
		resolved, err := app.resolveLink(bare, "")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resolved, capability) {
			t.Fatalf("resolved=%+v want=%+v", resolved, capability)
		}
		if !strings.Contains(stderr.String(), "enter the key string") {
			t.Fatalf("prompt=%q", stderr.String())
		}
	})

	t.Run("empty entered key", func(t *testing.T) {
		app, _, _ := newSemanticTestApp(strings.NewReader("\n"))
		if _, err := app.resolveLink(bare, ""); err == nil || err.Error() != missingCapabilityKeyDiagnostic {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("key input failure", func(t *testing.T) {
		app, _, _ := newSemanticTestApp(strings.NewReader(""))
		if _, err := app.resolveLink(bare, ""); !errors.Is(err, io.EOF) {
			t.Fatalf("error=%v want wrapped EOF", err)
		}
	})

	t.Run("invalid link has a stable typed usage diagnostic", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader(""))
		if _, parse := app.parseGetRequest([]string{"not-a-link"}); parse != requestParseUsageFailure {
			t.Fatalf("parse=%d want=%d", parse, requestParseUsageFailure)
		}
		if stderr.String() != "get: "+invalidCapabilityDiagnostic+"\n" {
			t.Fatalf("stderr=%q", stderr.String())
		}
		_, err := app.resolveLink("not-a-link", "")
		var typed *capabilityInputError
		if !errors.As(err, &typed) || typed.kind != capabilityInputInvalid {
			t.Fatalf("error=%#v", err)
		}
	})

	t.Run("relay address is mandatory", func(t *testing.T) {
		withoutRelay := newSemanticCapability(t)
		full, err := withoutRelay.URL("https://app.example")
		if err != nil {
			t.Fatal(err)
		}
		app, _, stderr := newSemanticTestApp(strings.NewReader(""))
		if _, parse := app.parseGetRequest([]string{full}); parse != requestParseUsageFailure {
			t.Fatalf("parse=%d want=%d", parse, requestParseUsageFailure)
		}
		if !strings.Contains(stderr.String(), "link has no relay address") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}

func TestTransferResultEventsPreserveExitCodePrecedence(t *testing.T) {
	published, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPartial)
	if err != nil {
		t.Fatal(err)
	}
	resumable, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
	if err != nil {
		t.Fatal(err)
	}
	needsAttention, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementFailed)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		result       transfer.JobResult
		cancel       bool
		admissionErr error
		wantCode     int
		wantText     []string
	}{
		{
			name: "success",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: published, SucceededFiles: 2,
				Progress: transfer.ReceiveProgressSnapshot{
					DiscoveredFiles: 2, DiscoveredBytes: 8, PublishedFiles: 2, PublishedBytes: 8,
					VerifiedBytes: 8, NewlyVerifiedBytes: 8,
					FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 2},
					Discovery:    transfer.DiscoveryComplete, CountersExact: true,
				},
			},
			wantCode: ExitOK, wantText: []string{"Download completed", "2 downloaded", "8 B"},
		},
		{
			name: "success cannot hide a typed collision",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: published, SucceededFiles: 1,
				Progress: transfer.ReceiveProgressSnapshot{
					DiscoveredFiles: 1, DiscoveredBytes: 4, PublishedFiles: 1, PublishedBytes: 4,
					VerifiedBytes: 4, NewlyVerifiedBytes: 4,
					FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1, CollisionFiles: 1},
					Discovery:    transfer.DiscoveryComplete, CountersExact: true,
				},
			},
			wantCode: ExitFailure,
			wantText: []string{"Download failed", "1 downloaded", "1 collision"},
		},
		{
			name: "source drift dominates partial completion",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePartial, Settlement: partial,
				SourceDriftFault: mustCLIFault(transferfault.NewSource(
					transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged,
				)),
			},
			wantCode: ExitDrift, wantText: []string{"finished partially", "source changed"},
		},
		{
			name:     "caller cancellation dominates generic termination",
			result:   transfer.JobResult{Outcome: transfer.DirectTreeOutcomePaused, Settlement: resumable, TerminationCause: errors.New("transfer stopped")},
			cancel:   true,
			wantCode: ExitFailure, wantText: []string{"Download paused", "interrupted"},
		},
		{
			name: "session failure dominates racing caller cancellation",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePaused, Settlement: resumable,
				TerminationCause: errors.Join(
					context.Canceled,
					errors.New("authenticated runtime failed"),
				),
				TerminationFault: mustCLIFault(transferfault.NewSession(
					transferfault.ScopeSessionTerminal, transferfault.SessionTransport,
				)),
			},
			cancel: true, wantCode: ExitNetwork, wantText: []string{"Download paused", "transfer session failed"},
		},
		{
			name: "relay admission failure remains network-visible",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePaused, Settlement: resumable, TerminationCause: errors.New("transfer stopped"),
			},
			admissionErr: errors.New("resume suspended relay failed"),
			wantCode:     ExitNetwork, wantText: []string{"Download paused", "unexpected error"},
		},
		{
			name: "missing explicit selection is usage",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePartial, Settlement: partial,
				Progress: transfer.ReceiveProgressSnapshot{
					Discovery: transfer.DiscoveryComplete, CountersExact: true,
				},
				SelectionResolutionFailure: errors.Join(transfer.ErrSelectionTargetMissing, errors.New("path: missing")),
			},
			wantCode: ExitUsage, wantText: []string{"finished partially", "selected content was not found"},
		},
		{
			name: "partial discovery cannot claim an explicit selection is missing",
			result: transfer.JobResult{
				Outcome:                    transfer.DirectTreeOutcomePartial,
				Settlement:                 partial,
				Progress:                   transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryOpen, CountersExact: true},
				SelectionResolutionFailure: transfer.ErrSelectionTargetMissing,
			},
			wantCode: ExitFailure, wantText: []string{"finished partially"},
		},
		{
			name: "settlement failure remains local and visible",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePaused, SettlementFailure: errors.New("checkpoint install failed"),
			},
			wantCode: ExitFailure,
			wantText: []string{"Download paused", "unexpected error"},
		},
		{
			name: "failed operation without a wrapped cause remains actionable",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomeFailed, Settlement: needsAttention,
			},
			wantCode: ExitFailure,
			wantText: []string{"Download failed", "unexpected error"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			runtime, stderr := newGetReportingRuntime(t, false, false)
			observation := getObservation{runtime: runtime}
			app := &App{}
			if code := app.reportTransferResultWithAdmission(
				ctx, newGetReportingCompletion(t, test.result), test.admissionErr, nil, nil,
				t.TempDir(), false, runtime.Clock().Now(), observation,
			); code != test.wantCode {
				t.Fatalf("exit=%d want=%d stderr=%q", code, test.wantCode, stderr.String())
			}
			runtime.Close()
			for _, expected := range test.wantText {
				if !strings.Contains(stderr.String(), expected) {
					t.Fatalf("stderr=%q does not contain %q", stderr.String(), expected)
				}
			}
			for _, retired := range []string{"result=", "phase=", "cause_class="} {
				if strings.Contains(stderr.String(), retired) {
					t.Fatalf("stderr=%q contains retired ledger %q", stderr.String(), retired)
				}
			}
		})
	}
}

func TestRelayContentAdmissionRejectsBrokenDependenciesWithoutStrandingContent(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	validClock := &semanticAdmissionClock{now: t0, timer: newSemanticAdmissionTimer()}
	validRelay := &semanticAdmissionSuspension{}

	for _, test := range []struct {
		name  string
		t0    time.Time
		clock receiverAdmissionClock
		relay receiverContentSuspension
	}{
		{name: "zero T0", clock: validClock, relay: validRelay},
		{name: "nil clock", t0: t0, relay: validRelay},
		{name: "nil relay suspension", t0: t0, clock: validClock},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newRelayContentAdmission(test.t0, test.clock, test.relay); !errors.Is(err, ErrInvalidReceiverAdmission) {
				t.Fatalf("error=%v want=%v", err, ErrInvalidReceiverAdmission)
			}
		})
	}

	t.Run("expired absolute deadline clamps to immediate", func(t *testing.T) {
		clock := &semanticAdmissionClock{
			now: t0.Add(receiverRelayAdmissionWindow + time.Second), timer: newSemanticAdmissionTimer(),
		}
		relay := &semanticAdmissionSuspension{}
		admission, err := newRelayContentAdmission(t0, clock, relay)
		if err != nil {
			t.Fatal(err)
		}
		delays := clock.recordedDelays()
		if !reflect.DeepEqual(delays, []time.Duration{0}) {
			t.Fatalf("delays=%v want=[0]", delays)
		}
		admission.Close()
	})

	t.Run("broken timer rolls suspension back", func(t *testing.T) {
		resumeErr := errors.New("rollback failed")
		clock := &semanticAdmissionClock{now: t0}
		relay := &semanticAdmissionSuspension{resumeErr: resumeErr}
		if _, err := newRelayContentAdmission(t0, clock, relay); !errors.Is(err, ErrInvalidReceiverAdmission) || !errors.Is(err, resumeErr) {
			t.Fatalf("error=%v must retain setup and rollback failures", err)
		}
		if resumes := relay.resumeCount(); resumes != 1 {
			t.Fatalf("resume calls=%d want=1", resumes)
		}
	})

	t.Run("broken timer contains rollback panic", func(t *testing.T) {
		clock := &semanticAdmissionClock{now: t0}
		relay := receiverContentSuspensionFunc(func() error { panic("rollback panic") })
		if _, err := newRelayContentAdmission(t0, clock, relay); !errors.Is(err, ErrInvalidReceiverAdmission) ||
			!errors.Is(err, errReceiverAdmissionResumePanics) {
			t.Fatalf("panic rollback error=%v", err)
		}
	})

	t.Run("unknown policy signals fail closed", func(t *testing.T) {
		clock := &semanticAdmissionClock{now: t0, timer: newSemanticAdmissionTimer()}
		relay := &semanticAdmissionSuspension{}
		admission, err := newRelayContentAdmission(t0, clock, relay)
		if err != nil {
			t.Fatal(err)
		}
		if err := admission.ObserveConnectionSize(transfer.ConnectionSizeClass(255)); !errors.Is(err, ErrInvalidReceiverAdmission) {
			t.Fatalf("selection error=%v", err)
		}
		if err := admission.ObservePeer(receiverPeerSignal(255)); !errors.Is(err, ErrInvalidReceiverAdmission) {
			t.Fatalf("peer error=%v", err)
		}
		if resumes := relay.resumeCount(); resumes != 0 {
			t.Fatalf("invalid signals resumed content %d time(s)", resumes)
		}
		admission.Close()
	})

	t.Run("injected clock is authoritative", func(t *testing.T) {
		clock := &semanticAdmissionClock{now: t0, timer: newSemanticAdmissionTimer()}
		app := &App{receiverClock: clock}
		if got := app.admissionClock(); got != clock {
			t.Fatalf("clock=%T want injected clock", got)
		}
	})

	t.Run("nil close is safe", func(t *testing.T) {
		var admission *relayContentAdmission
		admission.Close()
	})
}

func newSemanticTestApp(stdin io.Reader) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &App{Stdout: stdout, Stderr: stderr, Stdin: stdin}, stdout, stderr
}

func newSemanticCapability(t *testing.T, relays ...string) link.Link {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{0xa5}, link.ReadSecretBytes),
		privateKey.Public().(ed25519.PublicKey),
		relays,
	)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

type semanticAdmissionSuspension struct {
	mu        sync.Mutex
	resumeErr error
	resumes   int
}

func (relay *semanticAdmissionSuspension) Resume() error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	relay.resumes++
	return relay.resumeErr
}

func (relay *semanticAdmissionSuspension) resumeCount() int {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.resumes
}

type semanticAdmissionClock struct {
	mu     sync.Mutex
	now    time.Time
	timer  receiverAdmissionTimer
	delays []time.Duration
}

func (clock *semanticAdmissionClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *semanticAdmissionClock) NewTimer(delay time.Duration) receiverAdmissionTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.delays = append(clock.delays, delay)
	return clock.timer
}

func (clock *semanticAdmissionClock) recordedDelays() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.delays...)
}

type semanticAdmissionTimer struct {
	channel chan time.Time
	mu      sync.Mutex
	stopped bool
}

func newSemanticAdmissionTimer() *semanticAdmissionTimer {
	return &semanticAdmissionTimer{channel: make(chan time.Time)}
}

func (timer *semanticAdmissionTimer) C() <-chan time.Time { return timer.channel }

func (timer *semanticAdmissionTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}
