package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestParseInterleavedV2Flags(t *testing.T) {
	app := testApp("")
	flags := app.newFlagSet("share")
	relay := flags.String("relay", "", "")
	positionals, outcome := parseInterleaved(flags, []string{"first", "--relay", "ws://relay.example", "second"})
	if outcome != flagParseReady || *relay != "ws://relay.example" || strings.Join(positionals, ",") != "first,second" {
		t.Fatalf("parse result = %v %q %v", positionals, *relay, outcome)
	}
}

func TestGetRequestRejectsRetiredSuite(t *testing.T) {
	shareID := base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte{1}, link.SenderAuthenticatedShareIDBytes),
	)
	retiredKey := base64.RawURLEncoding.EncodeToString(
		append([]byte{0x01}, bytes.Repeat([]byte{2}, link.ReadSecretBytes)...),
	)
	encoded := "http://localhost:5173/" + shareID + "#" + retiredKey
	app := testApp("")
	if _, outcome := app.parseReceiveRequest(receiveGet, []string{encoded}); outcome != requestParseUsageFailure {
		t.Fatalf("retired suite parse outcome = %d", outcome)
	}
}

func TestTransferResultDriftClassification(t *testing.T) {
	for _, value := range []transferfault.Fault{
		mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)),
		mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionInvalidated)),
		mustCLIFault(transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, transferfault.CatalogDirectoryStale)),
	} {
		result, err := projectTestGetResult(engine.ReceiveSettlementInput{
			Result: transfer.JobResult{
				Outcome:          transfer.DirectTreeOutcomePartial,
				SourceDriftFault: value,
			},
			Destination: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Drift() != clievent.DriftSource || result.ExitCode() != clievent.ExitDrift {
			t.Fatalf("drift projection = %v/%v", result.Drift(), result.ExitCode())
		}
	}
	network, err := projectTestGetResult(engine.ReceiveSettlementInput{
		Result:       transfer.JobResult{Outcome: transfer.DirectTreeOutcomePaused},
		RuntimeError: errors.New("network"),
		Destination:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.Drift() != clievent.DriftNone || network.ExitCode() != clievent.ExitNetwork {
		t.Fatalf("network projection = %v/%v", network.Drift(), network.ExitCode())
	}
}

func mustCLIFault(value transferfault.Fault, err error) transferfault.Fault {
	if err != nil {
		panic(err)
	}
	return value
}

const cliShareCancellationTimeout = 10 * time.Second

func TestShareCancellationWaitsForCleanupAndRecordsTraces(t *testing.T) {
	for _, test := range []struct {
		name         string
		cleanupError error
		exitCode     int
	}{
		{name: "stopped", exitCode: ExitOK},
		{name: "cleanup failed", cleanupError: errors.New("relay stop failed"), exitCode: ExitNetwork},
	} {
		t.Run(test.name, func(t *testing.T) {
			testShareCancellation(t, test.cleanupError, test.exitCode)
		})
	}
}

func testShareCancellation(t *testing.T, cleanupError error, exitCode int) {
	t.Helper()
	relays := &cliShareRelays{
		stopped: make(chan struct{}), cleanupStarted: make(chan context.Context, 1),
		cleanupRelease: make(chan struct{}), cleanupError: cleanupError,
	}

	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("stop contract"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := &lockedTestBuffer{}
	stderr := &lockedTestBuffer{}
	userTrace := newRecordingUserTrace()
	userTracePath := filepath.Join(t.TempDir(), "share.ndjson")
	app := &App{
		Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""),
		engineConfig: engine.Config{Share: engine.ShareDependencies{
			Relays: func(context.Context, []relayprofile.Candidate) (engine.ShareRelays, error) {
				return relays, nil
			},
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		}},
		openUserTrace: func(
			target runtrace.Target,
			command clievent.Command,
			_ runtrace.Config,
			_ runtrace.Dependencies,
		) (userTraceRecorder, error) {
			expected, _ := runtrace.ExactFile(userTracePath)
			if target != expected || !filepath.IsAbs(userTracePath) || command != clievent.CommandShare {
				return nil, errors.New("unexpected user trace request")
			}
			return userTrace, nil
		},
	}
	shareContext, cancelShare := context.WithCancel(context.Background())
	result := make(chan int, 1)
	finished := make(chan struct{})
	releaseCleanup := sync.OnceFunc(func() { close(relays.cleanupRelease) })
	t.Cleanup(func() {
		cancelShare()
		releaseCleanup()
		select {
		case <-finished:
		case <-time.After(cliShareCancellationTimeout):
			t.Error("share command did not settle during cleanup")
		}
	})
	go func() {
		defer close(finished)
		result <- app.Run(shareContext, []string{"share", file, "--relay", relays.ReadyRelayURL(), "--front-url", "https://front.example", "--trace", userTracePath})
	}()
	waitRecordedReady(t, userTrace)
	cancelShare()
	select {
	case cleanupContext := <-relays.cleanupStarted:
		if err := cleanupContext.Err(); err != nil {
			t.Fatalf("share cancellation revoked cleanup: %v", err)
		}
		if _, bounded := cleanupContext.Deadline(); !bounded {
			t.Fatal("relay cleanup has no deadline")
		}
	case <-time.After(cliShareCancellationTimeout):
		t.Fatal("share cancellation did not start cleanup")
	}
	select {
	case code := <-result:
		t.Fatalf("share returned %d before cleanup completed", code)
	default:
	}
	releaseCleanup()
	select {
	case code := <-result:
		if code != exitCode {
			t.Fatalf("share cancellation exit=%d want=%d stderr=%q", code, exitCode, stderr.String())
		}
	case <-time.After(cliShareCancellationTimeout):
		t.Fatal("share did not complete explicit stop")
	}
	userTrace.mu.Lock()
	defer userTrace.mu.Unlock()
	stoppedEvents := 0
	stages := make(map[appobs.EngineTaskStage]int)
	for _, event := range userTrace.events {
		if lifecycle, ok := event.(appobs.EngineTaskObserved); ok {
			facts := lifecycle.Facts()
			stages[facts.Stage]++
			if facts.Stage == appobs.EngineTaskSettled && facts.CleanupFailure.Valid() != (cleanupError != nil) {
				t.Fatalf("cleanup failure lost from observation: %+v", facts)
			}
		}
		if stopped, ok := event.(clievent.SharingStopped); ok {
			stoppedEvents++
			if code, valid := stopped.Result().ExitCode().ProcessCode(); !valid || code != exitCode {
				t.Fatalf("user trace stop exit=%d valid=%v want=%d", code, valid, exitCode)
			}
		}
	}
	if stages[appobs.EngineTaskStopping] != 1 || stages[appobs.EngineTaskSettled] != 1 {
		t.Fatalf("share cleanup stages=%v", stages)
	}
	if stoppedEvents != 1 {
		t.Fatalf("user trace stop events=%d", stoppedEvents)
	}
}

type cliShareRelays struct {
	accepting      chan struct{}
	stopped        chan struct{}
	stopOnce       sync.Once
	cleanupStarted chan context.Context
	cleanupRelease chan struct{}
	cleanupError   error
}

func (relays *cliShareRelays) Accept(ctx context.Context) (protocolsession.FrameChannel, error) {
	if relays.accepting != nil {
		close(relays.accepting)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-relays.stopped:
		return nil, context.Canceled
	}
}

func (*cliShareRelays) WaitReady(context.Context) error { return nil }
func (*cliShareRelays) ReadyRelayURL() string           { return "https://relay.example" }
func (*cliShareRelays) Wake()                           {}
func (relays *cliShareRelays) StopRecovery() {
	relays.stopOnce.Do(func() { close(relays.stopped) })
}
func (relays *cliShareRelays) Cleanup(ctx context.Context) error {
	select {
	case relays.cleanupStarted <- ctx:
	default:
	}
	select {
	case <-relays.cleanupRelease:
		return relays.cleanupError
	case <-ctx.Done():
		return ctx.Err()
	}
}

type lockedTestBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedTestBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *lockedTestBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type recordingUserTrace struct {
	mu      sync.Mutex
	events  []clievent.Event
	health  chan clievent.TraceIncomplete
	changed chan struct{}
}

func newRecordingUserTrace() *recordingUserTrace {
	return &recordingUserTrace{
		health:  make(chan clievent.TraceIncomplete),
		changed: make(chan struct{}),
	}
}

func (trace *recordingUserTrace) Record(event clievent.Event) bool {
	trace.mu.Lock()
	trace.events = append(trace.events, event)
	close(trace.changed)
	trace.changed = make(chan struct{})
	trace.mu.Unlock()
	return true
}

func (*recordingUserTrace) ReportRejectionEvidenceLoss(uint64) bool { return true }

func (*recordingUserTrace) ReportUpstreamLoss(clievent.TraceLoss) bool { return true }

func (trace *recordingUserTrace) Health() <-chan clievent.TraceIncomplete { return trace.health }
func (*recordingUserTrace) Path() string                                  { return "trace.ndjson" }

func (*recordingUserTrace) Close() runtrace.Status { return runtrace.Status{Complete: true} }

func waitRecordedReady(t *testing.T, trace *recordingUserTrace) {
	t.Helper()
	timer := time.NewTimer(cliShareCancellationTimeout)
	defer timer.Stop()
	for {
		trace.mu.Lock()
		ready := false
		for _, event := range trace.events {
			if _, ok := event.(appobs.Ready); ok {
				ready = true
			}
		}
		changed := trace.changed
		trace.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("timed out waiting for sharing readiness observation")
		}
	}
}

func testApp(stdin string) *App {
	return &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader(stdin)}
}

func projectTestGetResult(input engine.ReceiveSettlementInput) (clievent.TransferResult, error) {
	result, err := engine.SettleReceive(input)
	if err != nil {
		return clievent.TransferResult{}, err
	}
	return commandprojection.ProjectReceiveResult(result)
}
