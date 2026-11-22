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

	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/internal/diaglog"
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
	if _, outcome := app.parseReceiveRequest(context.Background(), receiveGet, []string{encoded}); outcome != requestParseUsageFailure {
		t.Fatalf("retired suite parse outcome = %d", outcome)
	}
}

func TestTransferResultDriftClassification(t *testing.T) {
	for _, value := range []transferfault.Fault{
		mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)),
		mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionInvalidated)),
		mustCLIFault(transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, transferfault.CatalogDirectoryStale)),
	} {
		result, exit, err := projectTestGetResult(engine.ReceiveSettlementInput{
			Result: transfer.JobResult{
				Outcome:          transfer.DirectTreeOutcomePartial,
				SourceDriftFault: value,
			},
			Destination: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.SourceDrift || exit != ExitDrift {
			t.Fatalf("drift projection = %v/%d", result.SourceDrift, exit)
		}
	}
	network, exit, err := projectTestGetResult(engine.ReceiveSettlementInput{
		Result:       transfer.JobResult{Outcome: transfer.DirectTreeOutcomePaused},
		RuntimeError: errors.New("network"),
		Destination:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.SourceDrift || exit != ExitNetwork {
		t.Fatalf("network projection = %v/%d", network.SourceDrift, exit)
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
	userTrace := newFakeUserTrace(diaglog.RunStatus{})
	userTracePath := filepath.Join(t.TempDir(), "share.ndjson")
	app := &App{
		Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""),
		engineConfig: engine.Config{Share: engine.ShareDependencies{
			Relays: func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) {
				return relays, nil
			},
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		}},
		openUserTrace: func(target diaglog.Target, command string, _ diaglog.RunOptions) (runTraceRecorder, error) {
			expected, _ := diaglog.ExactFile(userTracePath)
			if target != expected || !filepath.IsAbs(userTracePath) || command != commandShare {
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
	stoppedEvents := 0
	stages := make(map[string]int)
	for _, rec := range userTrace.recorded() {
		switch rec.Event {
		case "engine_task_observed":
			var p struct {
				Stage          string `json:"stage"`
				CleanupFailure *struct {
					Code string `json:"code"`
				} `json:"cleanup_failure"`
			}
			decodeRecordPayload(t, rec.Payload, &p)
			stages[p.Stage]++
			if p.Stage == "task_settled" && (p.CleanupFailure != nil) != (cleanupError != nil) {
				t.Fatalf("cleanup failure lost from observation: %+v", p)
			}
		case "sharing_stopped":
			stoppedEvents++
			var p struct {
				ExitCode int `json:"exit_code"`
			}
			decodeRecordPayload(t, rec.Payload, &p)
			if p.ExitCode != exitCode {
				t.Fatalf("user trace stop exit=%d want=%d", p.ExitCode, exitCode)
			}
		}
	}
	if stages["task_stopping"] != 1 || stages["task_settled"] != 1 {
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

func waitRecordedReady(t *testing.T, trace *fakeUserTrace) {
	t.Helper()
	timer := time.NewTimer(cliShareCancellationTimeout)
	defer timer.Stop()
	for {
		trace.mu.Lock()
		ready := false
		for _, rec := range trace.records {
			if rec.Event == "ready" {
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

func projectTestGetResult(input engine.ReceiveSettlementInput) (humanoutput.TransferResult, int, error) {
	result, err := engine.SettleReceive(input)
	if err != nil {
		return humanoutput.TransferResult{}, 0, err
	}
	f, ok := engine.DescribeFailure(result.Err)
	return buildTransferResult(result, f, ok), exitCodeForClass(result.FailureClass), nil
}
